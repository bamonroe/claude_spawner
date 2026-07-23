package session

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH-native execution runs a session's turns on a remote host over SSH instead of
// forking claude locally. It is the unified transport of the SSH-native epic (see
// TODO.md): every host — including the local machine, reached as loopback — is just
// an entry in one connection pool, so there is a single execution path to maintain
// and a containerized server can drive the "real" host over SSH without a
// privileged host broker. This file is the foundation: the pooled, keepalive'd
// connection and the SSHExecutor that streams a turn's stream-json stdout back
// through the existing Executor/Proc seam. Config wiring, spawn-dialog host choice,
// Driver routing, over-SSH discovery, and robust tagged-process cancellation are
// separate later commits of the epic.

// SSHPool holds one authenticated *ssh.Client per host so turns don't pay a
// per-turn handshake: the expensive dial+auth happens once, then each turn opens a
// lightweight channel on the cached connection (≈free). A background keepalive per
// connection detects a dead link and drops the client, so the next turn
// transparently re-dials. Safe for concurrent use.
type SSHPool struct {
	cfg        SSHConfig
	hosts      *HostStore     // registry resolving a Session.Host name → connection details; may be nil
	ids        *IdentityStore // resolves a host's Identity → its server-side private key; may be nil
	knownHosts string         // known_hosts path; TrustHost/ForgetHost edit it and reload hostKey
	hostKey    ssh.HostKeyCallback
	mu         sync.Mutex
	clients    map[string]*ssh.Client
}

// NewSSHPool validates the global config (building the shared known_hosts
// verification) and returns a ready, empty pool. Per-host auth/user is built at
// dial time. hosts is the app-managed registry that resolves a Session.Host name to
// its address/user/port/key; nil (or a name absent from it) dials the name
// literally with the config defaults, preserving loopback/raw-hostname behavior.
func NewSSHPool(cfg SSHConfig, hosts *HostStore, ids *IdentityStore) (*SSHPool, error) {
	khPath, err := resolveKnownHostsPath(cfg.KnownHosts)
	if err != nil {
		return nil, err
	}
	cb, err := knownhosts.New(khPath)
	if err != nil {
		return nil, fmt.Errorf("ssh: load known_hosts %s: %w", khPath, err)
	}
	return &SSHPool{cfg: cfg, hosts: hosts, ids: ids, knownHosts: khPath, hostKey: cb, clients: map[string]*ssh.Client{}}, nil
}

// resolve maps a Session.Host name to dial details: a registry entry's
// address/user/port/key when present, else the name dialed literally with config
// defaults (loopback, raw hostnames, and tests).
func (p *SSHPool) resolve(name string) (addr, user, keyFile, password string, port int) {
	if p.hosts != nil {
		if h := p.hosts.Get(name); h != nil {
			a := h.Address
			if a == "" {
				a = name
			}
			user := h.User
			keyFile := h.KeyFile
			if h.Identity != "" && p.ids != nil {
				if id := p.ids.Get(h.Identity); id != nil {
					// The identity's user is a DEFAULT — a host's own User overrides it.
					if user == "" {
						user = id.User
					}
					password = id.Password
					// Only a key-bearing identity has a private key file on disk; a
					// password-only identity leaves keyFile empty (password auth).
					if id.PublicKey != "" {
						keyFile = p.ids.KeyPath(h.Identity)
					}
				}
			}
			return a, user, keyFile, password, h.Port
		}
	}
	return name, p.cfg.User, p.cfg.KeyFile, "", p.cfg.Port
}

// binFor returns the claude binary for a host: the registry entry's ClaudeBin, else
// the config default, else "claude".
func (p *SSHPool) binFor(name string) string {
	if p.hosts != nil {
		if h := p.hosts.Get(name); h != nil && h.ClaudeBin != "" {
			return h.ClaudeBin
		}
	}
	if p.cfg.Bin != "" {
		return p.cfg.Bin
	}
	return "claude"
}

// clientConfig builds a per-host *ssh.ClientConfig (user + auth vary by host; the
// known_hosts callback is shared).
func (p *SSHPool) clientConfig(user, keyFile, password string) (*ssh.ClientConfig, error) {
	login, err := p.cfg.resolveUser(user)
	if err != nil {
		return nil, err
	}
	auths, err := p.cfg.authMethods(keyFile, password)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            login,
		Auth:            auths,
		HostKeyCallback: p.hostKey,
		Timeout:         p.cfg.timeout(),
	}, nil
}

// client returns the cached connection for a host name, resolving it through the
// registry and dialing (and caching) on first use. Concurrent callers for the same
// cold host serialize on the pool lock, so exactly one dial happens. Cached by name
// so two names sharing an address keep independent entries.
func (p *SSHPool) client(name string) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[name]; c != nil {
		return c, nil
	}
	address, user, keyFile, password, port := p.resolve(name)
	ccfg, err := p.clientConfig(user, keyFile, password)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(address, portStr(port))
	c, err := p.dial(addr, ccfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	p.clients[name] = c
	go p.keepalive(name, c)
	return c, nil
}

// dial connects to addr, verifying the host key against known_hosts. On a
// known_hosts key mismatch it retries once with the host-key algorithms
// constrained to the type(s) stored for that host: unlike OpenSSH, the Go client
// doesn't bias host-key negotiation toward what we already trust, so a server
// offering (say) an RSA key when we recorded its ed25519 one would otherwise fail
// as a mismatch even though the ed25519 key is present and valid on both sides.
func (p *SSHPool) dial(addr string, ccfg *ssh.ClientConfig) (*ssh.Client, error) {
	c, err := ssh.Dial("tcp", addr, ccfg)
	if err == nil {
		return c, nil
	}
	var ke *knownhosts.KeyError
	if errors.As(err, &ke) && len(ke.Want) > 0 {
		retry := *ccfg
		retry.HostKeyAlgorithms = knownHostAlgos(ke.Want)
		return ssh.Dial("tcp", addr, &retry)
	}
	return nil, err
}

// knownHostAlgos returns the host-key algorithm names to offer given the keys
// known_hosts holds for a host, so negotiation selects a key type we've recorded.
// A stored RSA key also covers the rsa-sha2 signature variants a server may send.
func knownHostAlgos(keys []knownhosts.KnownKey) []string {
	seen := map[string]bool{}
	var algos []string
	add := func(a string) {
		if !seen[a] {
			seen[a] = true
			algos = append(algos, a)
		}
	}
	for _, k := range keys {
		t := k.Key.Type()
		add(t)
		if t == ssh.KeyAlgoRSA {
			add(ssh.KeyAlgoRSASHA256)
			add(ssh.KeyAlgoRSASHA512)
		}
	}
	return algos
}

// drop removes c from the cache (only if it's still the current client for host)
// and closes it, so the next client(host) re-dials. Idempotent.
func (p *SSHPool) drop(host string, c *ssh.Client) {
	p.mu.Lock()
	if p.clients[host] == c {
		delete(p.clients, host)
	}
	p.mu.Unlock()
	_ = c.Close()
}

// keepalive pings the connection until a request fails, then drops it. The ping is
// a global SSH request the server answers but otherwise ignores; a failure means
// the transport is gone, so we evict the client to force a fresh dial next turn.
func (p *SSHPool) keepalive(host string, c *ssh.Client) {
	t := time.NewTicker(sshKeepaliveInterval)
	defer t.Stop()
	for range t.C {
		if _, _, err := c.SendRequest("keepalive@spawner", true, nil); err != nil {
			p.drop(host, c)
			return
		}
	}
}

// Close tears down every pooled connection. Called on server shutdown.
func (p *SSHPool) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for h, c := range p.clients {
		_ = c.Close()
		delete(p.clients, h)
	}
	return nil
}

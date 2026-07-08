package session

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
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

const (
	// sshDefaultPort is the TCP port used when SSHConfig.Port is unset.
	sshDefaultPort = 22
	// sshDialTimeout is the default dial+handshake timeout (SSHConfig.Timeout==0).
	sshDialTimeout = 15 * time.Second
	// sshKeepaliveInterval is how often the pool pings a cached connection so a dead
	// link is detected (and dropped, forcing the next turn to re-dial) promptly.
	sshKeepaliveInterval = 30 * time.Second
)

// SSHConfig describes how to reach remote hosts for SSH-native execution. One
// config serves every host in the pool — a single login user, key, and known-hosts
// file — matching the typical single-account setup; per-host overrides can come
// later. Host keys are always verified (there is deliberately no insecure/skip
// mode): an unknown or changed host key fails the dial.
type SSHConfig struct {
	// User is the SSH login user; empty falls back to the current OS user.
	User string
	// Port is the SSH TCP port; 0 means sshDefaultPort (22).
	Port int
	// KeyFile is a private-key path for public-key auth; empty relies on the agent
	// (SSH_AUTH_SOCK) alone. At least one of the two must yield a usable key.
	KeyFile string
	// KnownHosts is the known_hosts path used to verify host keys; empty falls back
	// to ~/.ssh/known_hosts.
	KnownHosts string
	// Bin is the claude binary on the remote host; empty means "claude".
	Bin string
	// Timeout bounds the dial+handshake; 0 means sshDialTimeout.
	Timeout time.Duration
}

// hostKeyCallback builds strict host-key verification from known_hosts (shared by
// every pooled host — the known_hosts file is global). Built once at pool
// construction.
func (c SSHConfig) hostKeyCallback() (ssh.HostKeyCallback, error) {
	kh := c.KnownHosts
	if kh == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("ssh: locate home for known_hosts: %w", err)
		}
		kh = filepath.Join(home, ".ssh", "known_hosts")
	}
	cb, err := knownhosts.New(kh)
	if err != nil {
		return nil, fmt.Errorf("ssh: load known_hosts %s: %w", kh, err)
	}
	return cb, nil
}

// resolveUser returns the login user: the given per-host user, else the config
// default, else the server's current OS user.
func (c SSHConfig) resolveUser(host string) (string, error) {
	if host != "" {
		return host, nil
	}
	if c.User != "" {
		return c.User, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("ssh: resolve current user: %w", err)
	}
	return u.Username, nil
}

// authMethods gathers auth: the ssh-agent (if present) plus an explicit key file
// (the per-host key, else the config default). At least one usable method required.
func (c SSHConfig) authMethods(keyFile string) ([]ssh.AuthMethod, error) {
	if keyFile == "" {
		keyFile = c.KeyFile
	}
	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if keyFile != "" {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("ssh: read key %s: %w", keyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse key %s: %w", keyFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh: no auth method (set a key file or run an ssh-agent)")
	}
	return auths, nil
}

func (c SSHConfig) timeout() time.Duration {
	if c.Timeout == 0 {
		return sshDialTimeout
	}
	return c.Timeout
}

// portStr renders an SSH port, defaulting 0 to 22.
func portStr(p int) string {
	if p == 0 {
		p = sshDefaultPort
	}
	return strconv.Itoa(p)
}

// SSHPool holds one authenticated *ssh.Client per host so turns don't pay a
// per-turn handshake: the expensive dial+auth happens once, then each turn opens a
// lightweight channel on the cached connection (≈free). A background keepalive per
// connection detects a dead link and drops the client, so the next turn
// transparently re-dials. Safe for concurrent use.
type SSHPool struct {
	cfg     SSHConfig
	hosts   *HostStore     // registry resolving a Session.Host name → connection details; may be nil
	ids     *IdentityStore // resolves a host's Identity → its server-side private key; may be nil
	hostKey ssh.HostKeyCallback
	mu      sync.Mutex
	clients map[string]*ssh.Client
}

// NewSSHPool validates the global config (building the shared known_hosts
// verification) and returns a ready, empty pool. Per-host auth/user is built at
// dial time. hosts is the app-managed registry that resolves a Session.Host name to
// its address/user/port/key; nil (or a name absent from it) dials the name
// literally with the config defaults, preserving loopback/raw-hostname behavior.
func NewSSHPool(cfg SSHConfig, hosts *HostStore, ids *IdentityStore) (*SSHPool, error) {
	cb, err := cfg.hostKeyCallback()
	if err != nil {
		return nil, err
	}
	return &SSHPool{cfg: cfg, hosts: hosts, ids: ids, hostKey: cb, clients: map[string]*ssh.Client{}}, nil
}

// resolve maps a Session.Host name to dial details: a registry entry's
// address/user/port/key when present, else the name dialed literally with config
// defaults (loopback, raw hostnames, and tests).
func (p *SSHPool) resolve(name string) (addr, user, keyFile string, port int) {
	if p.hosts != nil {
		if h := p.hosts.Get(name); h != nil {
			a := h.Address
			if a == "" {
				a = name
			}
			// A managed identity supersedes a raw KeyFile: use its server-side key.
			keyFile := h.KeyFile
			if h.Identity != "" && p.ids != nil {
				keyFile = p.ids.KeyPath(h.Identity)
			}
			return a, h.User, keyFile, h.Port
		}
	}
	return name, p.cfg.User, p.cfg.KeyFile, p.cfg.Port
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
func (p *SSHPool) clientConfig(user, keyFile string) (*ssh.ClientConfig, error) {
	login, err := p.cfg.resolveUser(user)
	if err != nil {
		return nil, err
	}
	auths, err := p.cfg.authMethods(keyFile)
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
	address, user, keyFile, port := p.resolve(name)
	ccfg, err := p.clientConfig(user, keyFile)
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

// SSHExecutor runs a session's turns on a remote host over SSH, streaming the same
// stream-json stdout the local executors produce, so Driver.Turn parses it without
// knowing the turn ran remotely. The target host is Session.Host — an explicit
// name (LocalHost for loopback), never an implicit default; the exact claude argv
// is the one Driver.Turn already builds.
type SSHExecutor struct {
	// Pool supplies the pooled, keepalive'd connection per host.
	Pool *SSHPool
	// Bin overrides the remote claude binary for ALL hosts; empty defers to the
	// per-host registry binary (Pool.binFor), then the config default, then "claude".
	Bin string
}

// Start opens a channel on the session's host connection and runs claude there. If
// the cached connection has died since the last turn, it drops it and re-dials once
// before failing, so a link that dropped between turns heals transparently.
func (e SSHExecutor) Start(ctx context.Context, s *Session, args []string) (Proc, error) {
	host := s.Host
	if host == "" {
		// SSH-native execution never defaults to the local box: a host-target
		// session must name an explicit host (LocalHost for loopback). This is what
		// lets a deployment drive only remote hosts — a hostless session is a bug,
		// not an implicit "run it here".
		return nil, fmt.Errorf("session %q has no host set", s.Name)
	}
	bin := e.Bin
	if bin == "" {
		bin = e.Pool.binFor(host)
	}
	client, err := e.Pool.client(host)
	if err != nil {
		return nil, err
	}
	proc, err := e.run(ctx, client, s.Dir, bin, args)
	if err != nil {
		// The pooled connection may have died since the last turn; evict and re-dial
		// once. A fresh client that still fails is a real error.
		e.Pool.drop(host, client)
		client, derr := e.Pool.client(host)
		if derr != nil {
			return nil, err
		}
		proc, err = e.run(ctx, client, s.Dir, bin, args)
	}
	return proc, err
}

// run opens one SSH session (channel) on client, launches the remote claude in the
// session's directory, and wires ctx-cancel to stop it. The returned Proc streams
// the remote stdout and Waits on the remote exit.
func (e SSHExecutor) run(ctx context.Context, client *ssh.Client, dir, bin string, args []string) (Proc, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	// Read stderr out of band from the stream-json stdout: the remote command echoes
	// its process-group id there (see cancelableCommand) so a cancel can kill the
	// whole group. We also drain the rest so a chatty claude stderr can't block it.
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	if err := sess.Start(cancelableCommand(remoteCommand(dir, bin, args))); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("start remote claude: %w", err)
	}
	var pmu sync.Mutex
	pgid := 0
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, sshPGIDSentinel); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					pmu.Lock()
					pgid = n
					pmu.Unlock()
				}
			}
		}
	}()
	// On abort, kill the remote process GROUP over a second channel on the same
	// connection (handshake-free), so claude AND any tool child it spawned die — the
	// remote analogue of the host executor's process-group SIGKILL. The signal+close
	// is a belt-and-suspenders fallback for the rare cancel that races the pgid
	// readout (a real turn runs for seconds, so the pgid is long since captured).
	stop := context.AfterFunc(ctx, func() {
		pmu.Lock()
		g := pgid
		pmu.Unlock()
		if g > 0 {
			killRemoteGroup(client, g)
		}
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
	})
	return &sshProc{sess: sess, stdout: stdout, stop: stop}, nil
}

// sshPGIDSentinel prefixes the line cancelableCommand writes to stderr carrying the
// remote process-group id, so run() can parse it back out of the stderr stream.
const sshPGIDSentinel = "__spawner_pgid__ "

// cancelableCommand wraps a turn's inner "cd … && exec claude …" so an abort can
// kill the whole remote process tree. setsid puts the command in a fresh session /
// process group (whose pgid is the wrapper shell's pid); the shell echoes that pgid
// on stderr and then execs into inner, so claude replaces the shell keeping the same
// pgid and every tool child it spawns inherits it. A cancel then kills -pgid to take
// the group down together. No PTY is requested, so the stream-json stdout stays
// clean (the pgid rides stderr).
func cancelableCommand(inner string) string {
	return "setsid sh -c " + shellQuote("echo "+sshPGIDSentinel+"$$ 1>&2; "+inner)
}

// killRemoteGroup opens a fresh channel on the live connection and SIGKILLs the
// remote process group, matching the host executor's group-kill-on-abort semantics.
// Best-effort: a failure (already exited, connection gone) is ignored.
func killRemoteGroup(client *ssh.Client, pgid int) {
	s, err := client.NewSession()
	if err != nil {
		return
	}
	defer s.Close()
	_ = s.Run(fmt.Sprintf("kill -s KILL -%d", pgid))
}

// sshProc adapts an *ssh.Session to Proc.
type sshProc struct {
	sess   *ssh.Session
	stdout io.Reader
	stop   func() bool // cancels the ctx AfterFunc; from context.AfterFunc
}

func (p *sshProc) Stdout() io.Reader { return p.stdout }

func (p *sshProc) Wait() error {
	if p.stop != nil {
		p.stop() // release the cancel hook; no-op if it already ran
	}
	err := p.sess.Wait()
	_ = p.sess.Close()
	return err
}

// remoteCommand builds the POSIX-sh command run on the remote host for one turn:
// change into the session directory, then exec claude with the given args. Every
// component is single-quoted so a prompt containing spaces, quotes, or newlines
// reaches the remote claude verbatim.
func remoteCommand(dir, bin string, args []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec ")
	b.WriteString(shellQuote(bin))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded single
// quotes as the standard '\” sequence, so arbitrary text survives the remote shell
// unchanged.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

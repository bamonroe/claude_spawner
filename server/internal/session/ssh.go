package session

import (
	"context"
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

// clientConfig builds the *ssh.ClientConfig once (shared by every pooled host):
// resolves the login user, gathers auth methods (agent first, then an explicit key
// file), and installs strict host-key verification from known_hosts.
func (c SSHConfig) clientConfig() (*ssh.ClientConfig, error) {
	login := c.User
	if login == "" {
		u, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("ssh: resolve current user: %w", err)
		}
		login = u.Username
	}

	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if c.KeyFile != "" {
		key, err := os.ReadFile(c.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("ssh: read key %s: %w", c.KeyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse key %s: %w", c.KeyFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh: no auth method (set a key file or run an ssh-agent)")
	}

	kh := c.KnownHosts
	if kh == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("ssh: locate home for known_hosts: %w", err)
		}
		kh = filepath.Join(home, ".ssh", "known_hosts")
	}
	hostKey, err := knownhosts.New(kh)
	if err != nil {
		return nil, fmt.Errorf("ssh: load known_hosts %s: %w", kh, err)
	}

	timeout := c.Timeout
	if timeout == 0 {
		timeout = sshDialTimeout
	}
	return &ssh.ClientConfig{
		User:            login,
		Auth:            auths,
		HostKeyCallback: hostKey,
		Timeout:         timeout,
	}, nil
}

func (c SSHConfig) port() string {
	p := c.Port
	if p == 0 {
		p = sshDefaultPort
	}
	return strconv.Itoa(p)
}

// sshPool holds one authenticated *ssh.Client per host so turns don't pay a
// per-turn handshake: the expensive dial+auth happens once, then each turn opens a
// lightweight channel on the cached connection (≈free). A background keepalive per
// connection detects a dead link and drops the client, so the next turn
// transparently re-dials. Safe for concurrent use.
type sshPool struct {
	cfg     SSHConfig
	ccfg    *ssh.ClientConfig
	mu      sync.Mutex
	clients map[string]*ssh.Client
}

// newSSHPool validates the config (building the shared client config, which loads
// keys and known_hosts) and returns a ready, empty pool. Connections are dialed
// lazily on first use per host.
func newSSHPool(cfg SSHConfig) (*sshPool, error) {
	ccfg, err := cfg.clientConfig()
	if err != nil {
		return nil, err
	}
	return &sshPool{cfg: cfg, ccfg: ccfg, clients: map[string]*ssh.Client{}}, nil
}

// client returns the cached connection for host, dialing and caching one (and
// starting its keepalive) on first use. Concurrent callers for the same cold host
// serialize on the pool lock, so exactly one dial happens.
func (p *sshPool) client(host string) (*ssh.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.clients[host]; c != nil {
		return c, nil
	}
	addr := net.JoinHostPort(host, p.cfg.port())
	c, err := ssh.Dial("tcp", addr, p.ccfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	p.clients[host] = c
	go p.keepalive(host, c)
	return c, nil
}

// drop removes c from the cache (only if it's still the current client for host)
// and closes it, so the next client(host) re-dials. Idempotent.
func (p *sshPool) drop(host string, c *ssh.Client) {
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
func (p *sshPool) keepalive(host string, c *ssh.Client) {
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
func (p *sshPool) Close() error {
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
// knowing the turn ran remotely. The target host is Session.Host (empty =
// loopback); the exact claude argv is the one Driver.Turn already builds.
type SSHExecutor struct {
	// Pool supplies the pooled, keepalive'd connection per host.
	Pool *sshPool
	// Bin overrides the remote claude binary; empty falls back to the pool config's
	// Bin, then "claude".
	Bin string
}

func (e SSHExecutor) bin() string {
	if e.Bin != "" {
		return e.Bin
	}
	if e.Pool != nil && e.Pool.cfg.Bin != "" {
		return e.Pool.cfg.Bin
	}
	return "claude"
}

// Start opens a channel on the session's host connection and runs claude there. If
// the cached connection has died since the last turn, it drops it and re-dials once
// before failing, so a link that dropped between turns heals transparently.
func (e SSHExecutor) Start(ctx context.Context, s *Session, args []string) (Proc, error) {
	host := s.Host
	if host == "" {
		host = "localhost"
	}
	client, err := e.Pool.client(host)
	if err != nil {
		return nil, err
	}
	proc, err := e.run(ctx, client, s.Dir, args)
	if err != nil {
		// The pooled connection may have died since the last turn; evict and re-dial
		// once. A fresh client that still fails is a real error.
		e.Pool.drop(host, client)
		client, derr := e.Pool.client(host)
		if derr != nil {
			return nil, err
		}
		proc, err = e.run(ctx, client, s.Dir, args)
	}
	return proc, err
}

// run opens one SSH session (channel) on client, launches the remote claude in the
// session's directory, and wires ctx-cancel to stop it. The returned Proc streams
// the remote stdout and Waits on the remote exit.
func (e SSHExecutor) run(ctx context.Context, client *ssh.Client, dir string, args []string) (Proc, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	if err := sess.Start(remoteCommand(dir, e.bin(), args)); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("start remote claude: %w", err)
	}
	// On abort, signal the remote command and close the channel. NOTE: without a PTY
	// many sshd builds won't propagate the signal to the process, so this is only
	// best-effort cancellation for now — robust tagged-process-group kill over a
	// second channel is the next commit of the epic (see TODO.md).
	stop := context.AfterFunc(ctx, func() {
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
	})
	return &sshProc{sess: sess, stdout: stdout, stop: stop}, nil
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
// quotes as the standard '\'' sequence, so arbitrary text survives the remote shell
// unchanged.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

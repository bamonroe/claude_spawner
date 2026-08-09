package session

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSH-native execution runs a session's turns on a remote host over SSH instead of
// forking claude locally. It is the unified transport of the SSH-native epic (see
// TODO.toml): every host — including the local machine, reached as loopback — is just
// an entry in one connection pool, so there is a single execution path to maintain
// and a containerized server can drive the "real" host over SSH without a
// privileged host broker. This file is the foundation: the pooled, keepalive'd
// connection and the SSHExecutor that streams a turn's stream-json stdout back
// through the existing Executor/Proc seam. Config wiring, spawn-dialog host choice,
// Driver routing, over-SSH discovery, and robust tagged-process cancellation are
// separate later commits of the epic.

// SSHPool holds authenticated *ssh.Clients per host so turns don't pay a per-turn
// handshake: the expensive dial+auth happens once, then each turn opens a
// lightweight channel on a cached connection (≈free). A background keepalive per
// connection detects a dead link and drops that connection, so the next turn
// transparently re-dials. Safe for concurrent use.
type SSHPool struct {
	cfg        SSHConfig
	hosts      *HostStore     // registry resolving a Session.Host name → connection details; may be nil
	ids        *IdentityStore // resolves a host's Identity → its server-side private key; may be nil
	knownHosts string         // known_hosts path; TrustHost/ForgetHost edit it and reload hostKey
	hostKey    ssh.HostKeyCallback
	mu         sync.Mutex            // guards the entries map ONLY — never held across a dial
	entries    map[string]*poolEntry // one per host name
}

// poolEntry is one host's cached connections plus the lock that serializes dials
// FOR THAT HOST. The per-host lock is the whole point: dialing an unreachable host
// blocks for the full dial timeout, and when that wait happened under a pool-wide
// lock it stalled every other host's callers too. One offline machine in the
// registry (a laptop that's asleep, a box off the tailnet) would freeze all
// loopback work — history reads, discovery, browsing — for 15 s at a stretch,
// which read as "the server is broken" with nothing in the log to show for it.
// Holding p.mu only for the map lookup keeps a dead host's cost local to that host.
//
// A host holds a SLICE of connections, not one. A single TCP connection is capped
// at the peer's MaxSessions, so making it the unit of concurrency meant the host's
// total parallelism was that one ceiling: past it every caller queued, however idle
// the machine actually was. Each connection carries its own channel budget and
// callers are handed the least-loaded one; when all of them are saturated the pool
// dials an ADDITIONAL connection (up to sshMaxConnsPerHost) instead of making the
// caller wait. Only at the cap does anyone block. One dead or busy connection is
// then a local problem — it is evicted alone, and the host's other connections keep
// working.
type poolEntry struct {
	mu    sync.Mutex // serializes dials for this host; may be held for the dial timeout
	conns []*pooledConn
	// Negative dial cache, guarded by mu. A host that fails to dial is marked down
	// until downUntil; every caller in that window gets downErr immediately rather
	// than serializing behind another full dial timeout. backoff doubles per
	// consecutive failure (sshDialBackoffMin..Max) and all three reset on success.
	downErr   error
	downUntil time.Time
	backoff   time.Duration
}

// down reports whether the host is inside its negative-cache window, and the
// error to fail fast with. Caller must hold e.mu.
func (e *poolEntry) down(now time.Time) (error, bool) {
	if e.downErr == nil || !now.Before(e.downUntil) {
		return nil, false
	}
	return e.downErr, true
}

// markDown records a failed dial, widens the backoff window, and returns the
// error every caller in that window will see — it names the host and when the
// pool will try again, so a stalled sweep is attributable instead of silent.
// Caller holds e.mu.
func (e *poolEntry) markDown(now time.Time, host string, cause error) error {
	if e.backoff <= 0 {
		e.backoff = sshDialBackoffMin
	} else {
		e.backoff = min(e.backoff*2, sshDialBackoffMax)
	}
	e.downUntil = now.Add(e.backoff)
	e.downErr = fmt.Errorf("host %q unreachable, not retrying for %s: %w", host, e.backoff, cause)
	return e.downErr
}

// markUp clears the negative cache after a successful dial. Caller holds e.mu.
func (e *poolEntry) markUp() {
	e.downErr = nil
	e.downUntil = time.Time{}
	e.backoff = 0
}

// Down reports whether a host is currently inside its negative-dial-cache window,
// and the error to fail with. It is the cheap, NON-BLOCKING "is this host known
// unreachable?" question: unlike client(), it never waits on the entry lock, so a
// caller can ask while another goroutine is mid-dial and get "unknown" rather than
// stalling for the dial timeout. Used by work that must degrade instead of hang —
// a session delete against an offline box defers its remote purge (see PurgeQueue)
// rather than grinding through a dial timeout per file.
//
// Not-down is not a promise of reachability: a cold host, or one being dialed right
// now, answers false. Treat true as "definitely down", false as "try it".
func (p *SSHPool) Down(name string) (error, bool) {
	if p == nil {
		return nil, false
	}
	if name == "" {
		name = LocalHost
	}
	e := p.entry(name)
	if !e.mu.TryLock() {
		return nil, false // a dial is in flight; don't wait on it just to answer
	}
	defer e.mu.Unlock()
	if len(e.conns) > 0 {
		return nil, false
	}
	return e.down(time.Now())
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
	return &SSHPool{cfg: cfg, hosts: hosts, ids: ids, knownHosts: khPath, hostKey: cb, entries: map[string]*poolEntry{}}, nil
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

// client returns a cached connection for a host name, resolving it through the
// registry and dialing (and caching) on first use. Concurrent callers for the same
// cold host serialize on that host's entry lock, so exactly one dial happens —
// while callers for OTHER hosts proceed unblocked. Cached by name so two names
// sharing an address keep independent entries.
//
// It answers "give me A connection to this host", ignoring load; the load-aware
// choice belongs to openChannel, which is where a channel slot is actually taken.
func (p *SSHPool) client(name string) (*pooledConn, error) {
	e := p.entry(name)
	// Per-host lock only: a slow or hanging dial to THIS host must not stall
	// callers of any other host. See poolEntry.
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := leastLoaded(e.conns); c != nil {
		return c, nil
	}
	return p.dialLocked(name, e)
}

// dialLocked adds one more connection for a host and caches it. Caller holds e.mu,
// which is what serializes dials per host — including the extra connections opened
// when every existing one is saturated. Returns the pool's fail-fast error while
// the host sits in its negative-dial-cache window.
func (p *SSHPool) dialLocked(name string, e *poolEntry) (*pooledConn, error) {
	// Fail fast while the host is known down: re-dialing an unreachable machine
	// costs a full sshDialTimeout per caller, serialized on this very lock.
	if err, ok := e.down(time.Now()); ok {
		return nil, err
	}
	address, user, keyFile, password, port := p.resolve(name)
	ccfg, err := p.clientConfig(user, keyFile, password)
	if err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(address, portStr(port))
	raw, err := p.dial(addr, ccfg)
	if err != nil {
		return nil, e.markDown(time.Now(), name, fmt.Errorf("ssh dial %s: %w", addr, err))
	}
	e.markUp()
	c := newPooledConn(raw)
	e.conns = append(e.conns, c)
	go p.keepalive(name, c)
	return c, nil
}

// entry returns the per-host pool slot, creating it on first use. The pool-wide
// lock is held only for this map lookup — never across a dial.
func (p *SSHPool) entry(name string) *poolEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[name]
	if e == nil {
		e = &poolEntry{}
		p.entries[name] = e
	}
	return e
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

// pooledConn is a pooled connection plus the count of SSH channels currently open
// on it. The count exists so eviction can be made SAFE for work already in flight:
// dropping a client used to close the transport immediately, which killed every
// other channel sharing it — a background probe's failure could tear down the
// stream of a turn that was running perfectly well.
//
// So a drop is split in two. Evicting the connection from the pool (nobody new
// gets it) is immediate and always correct. CLOSING it is deferred until the last
// in-flight channel is done: a genuinely dead transport fails those channels
// anyway, so nothing is kept alive that isn't, while a healthy connection someone
// is mid-turn on survives to finish. Every channel is opened through openChannel,
// so the count is a property of the pool rather than a rule call sites must
// remember.
type pooledConn struct {
	*ssh.Client

	// chans bounds concurrent SSH channels on THIS connection, below the peer's
	// MaxSessions ceiling — see ssh_channels.go. It belongs to the connection
	// because that ceiling is per-connection: a second connection to the same host
	// brings its own allowance, which is the whole reason the pool opens one.
	chans channelBudget

	// lastUsed is when a channel was last opened on this connection, stamped under
	// mu. It is what lets an idle EXTRA connection be reaped later without touching
	// one that is merely between round trips.
	lastUsed time.Time

	// closeConn tears down the transport. A field rather than a direct
	// Client.Close() call so the in-flight accounting can be tested without a
	// live sshd; newPooledConn wires it to the real close.
	closeConn func() error

	mu       sync.Mutex
	inflight int
	retired  bool // evicted from the pool; close once idle
	closed   bool // closeConn already called; a repeat drop must not re-close
}

// newPooledConn wraps a freshly dialed client for the pool.
func newPooledConn(c *ssh.Client) *pooledConn {
	return &pooledConn{Client: c, closeConn: c.Close}
}

// hold records a channel opening on this connection.
func (c *pooledConn) hold() {
	c.mu.Lock()
	c.inflight++
	c.lastUsed = time.Now()
	c.mu.Unlock()
}

// live reports whether the connection is still eligible for new channels — a
// retired one has been evicted and is only waiting out its in-flight work.
func (c *pooledConn) live() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.retired
}

// done records a channel closing, closing the transport if this was the last
// channel on an already-retired connection.
func (c *pooledConn) done() {
	c.mu.Lock()
	c.inflight--
	last := c.retired && c.inflight <= 0 && !c.closed
	c.closed = c.closed || last
	c.mu.Unlock()
	if last {
		_ = c.closeConn()
	}
}

// retire marks the connection evicted and closes it once it is idle. Idempotent.
func (c *pooledConn) retire() {
	c.mu.Lock()
	c.retired = true
	idle := c.inflight <= 0 && !c.closed
	c.closed = c.closed || idle
	c.mu.Unlock()
	if idle {
		_ = c.closeConn()
	}
}

// drop removes ONE connection from the host's cache so a later caller re-dials,
// and closes it once its in-flight channels finish — see pooledConn. Idempotent.
//
// Only the offending connection goes: that is the point of holding several. A
// transport error on one link says nothing about the host's other links, and
// evicting them all would take down turns streaming perfectly happily.
func (p *SSHPool) drop(host string, c *pooledConn) {
	if c == nil {
		return // the attempt never got as far as a connection (e.g. the dial failed)
	}
	e := p.entry(host)
	e.mu.Lock()
	e.conns = slices.DeleteFunc(e.conns, func(x *pooledConn) bool { return x == c })
	e.mu.Unlock()
	c.retire()
}

// keepalive pings the connection until a request fails, then drops it. The ping is
// a global SSH request the server answers but otherwise ignores; a failure means
// the transport is gone, so we evict the client to force a fresh dial next turn.
func (p *SSHPool) keepalive(host string, c *pooledConn) {
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
	entries := make([]*poolEntry, 0, len(p.entries))
	for h, e := range p.entries {
		entries = append(entries, e)
		delete(p.entries, h)
	}
	p.mu.Unlock()
	// Close outside the pool lock: an entry mid-dial holds its own lock for the
	// dial timeout, and shutdown mustn't serialize every host behind that.
	for _, e := range entries {
		e.mu.Lock()
		for _, c := range e.conns {
			_ = c.closeConn() // shutdown: force, don't wait on in-flight channels
		}
		e.conns = nil
		e.mu.Unlock()
	}
	return nil
}

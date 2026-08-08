package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// A host that accepts TCP but never completes the SSH handshake makes client()
// block for the full dial timeout. That wait must stay local to that host: a
// caller for a DIFFERENT host has to proceed immediately. Before the per-host
// entry lock this wait was taken under the pool-wide mutex, so one unreachable
// machine in the registry stalled every other host's callers — including all
// loopback work — and the server looked hung for no visible reason.
func TestSlowDialDoesNotBlockOtherHosts(t *testing.T) {
	// A listener that accepts and then goes silent: the SSH handshake hangs.
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	go func() {
		for {
			c, err := blackhole.Accept()
			if err != nil {
				return
			}
			defer c.Close() //nolint:revive // held open for the test's lifetime on purpose
		}
	}()

	// A port with nothing listening fails fast, which is what the second caller
	// needs: it proves it wasn't queued behind the first host's stalled dial.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := closed.Addr().String()
	closed.Close()

	hosts := &HostStore{}
	pool := &SSHPool{
		cfg:     SSHConfig{User: "nobody", Timeout: 10 * time.Second},
		hosts:   hosts,
		entries: map[string]*poolEntry{},
		hostKey: func(string, net.Addr, ssh.PublicKey) error { return nil },
	}

	stuck := make(chan struct{})
	go func() {
		defer close(stuck)
		_, _ = pool.client(blackhole.Addr().String()) // blocks on the handshake
	}()

	// Give the stalled dial a moment to take its host's lock.
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = pool.client(deadAddr) // must fail fast, not queue behind the above
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a dial to a second host blocked behind the first host's stalled dial")
	}
}

// Two names for the same host keep independent entries, and entry() is stable
// across calls so the per-host lock actually serializes that host's dials.
func TestEntryIsPerNameAndStable(t *testing.T) {
	p := &SSHPool{entries: map[string]*poolEntry{}}
	a1, a2 := p.entry("alpha"), p.entry("alpha")
	if a1 != a2 {
		t.Fatal("entry() must return the same slot for a name")
	}
	if p.entry("beta") == a1 {
		t.Fatal("distinct names must get distinct slots")
	}
}

// After a failed dial the pool must not re-dial that host for a backoff window:
// every caller (digest sweep, chainSig, context usage, a turn) would otherwise pay
// a fresh sshDialTimeout, serialized on the host's entry lock — the signature of
// 90-160s digest sweeps against one offline machine.
func TestFailedDialIsNegativeCachedAndFailsFast(t *testing.T) {
	// A port with nothing listening: the dial fails, which is all the negative
	// cache needs. Counting connection attempts proves the second call never dialed.
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portText, _ := net.SplitHostPort(closed.Addr().String())
	port, _ := strconv.Atoi(portText)
	closed.Close()

	pool := &SSHPool{
		cfg:     SSHConfig{User: "nobody", Port: port, Timeout: time.Second, KeyFile: testKeyFile(t)},
		entries: map[string]*poolEntry{},
		hostKey: func(string, net.Addr, ssh.PublicKey) error { return nil },
	}

	first, err := pool.client(host)
	if err == nil {
		t.Fatal("expected the first dial to an unreachable host to fail")
	}
	_ = first
	e := pool.entry(host)
	if e.downErr == nil {
		t.Fatal("a failed dial must mark the host down in the negative cache")
	}
	cached := e.downErr

	_, err2 := pool.client(host)
	if err2 == nil {
		t.Fatal("expected the cached failure to be returned")
	}
	// The very same error value: a re-dial would have produced a fresh one.
	if err2 != cached {
		t.Fatalf("second call re-dialed instead of serving the negative cache: %v", err2)
	}
	if !strings.Contains(err2.Error(), host) {
		t.Fatalf("the fail-fast error must name the host, got: %v", err2)
	}
	if e.backoff != sshDialBackoffMin {
		t.Fatalf("backoff = %s after one failure, want %s (the second call must not widen it)", e.backoff, sshDialBackoffMin)
	}
}

// The backoff window doubles per consecutive failure up to the cap, expires so a
// recovered host is retried, and resets on a success.
func TestNegativeDialCacheBackoffLifecycle(t *testing.T) {
	e := &poolEntry{}
	now := time.Now()

	if _, down := e.down(now); down {
		t.Fatal("a fresh entry must not be marked down")
	}
	e.markDown(now, "mom", errors.New("boom"))
	if e.backoff != sshDialBackoffMin {
		t.Fatalf("first failure backoff = %s, want %s", e.backoff, sshDialBackoffMin)
	}
	if _, down := e.down(now); !down {
		t.Fatal("entry must be down inside its window")
	}
	// The window expires, so a host that came back is retried.
	if _, down := e.down(now.Add(sshDialBackoffMin + time.Second)); down {
		t.Fatal("entry must be retried once the backoff window expires")
	}

	e.markDown(now, "mom", errors.New("boom"))
	if e.backoff != 2*sshDialBackoffMin {
		t.Fatalf("second failure backoff = %s, want %s", e.backoff, 2*sshDialBackoffMin)
	}
	for range 10 {
		e.markDown(now, "mom", errors.New("boom"))
	}
	if e.backoff != sshDialBackoffMax {
		t.Fatalf("backoff = %s, want it capped at %s", e.backoff, sshDialBackoffMax)
	}

	e.markUp()
	if _, down := e.down(now); down {
		t.Fatal("a successful dial must clear the negative cache")
	}
	if e.backoff != 0 {
		t.Fatalf("backoff = %s after success, want 0", e.backoff)
	}
}

// testKeyFile writes a throwaway ed25519 private key so the pool has a usable
// auth method and reaches the dial (rather than failing config-side first).
func testKeyFile(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "test")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

package session

import (
	"net"
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

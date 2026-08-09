package session

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// TestShouldRedial pins the pool's drop policy. The two false cases are the ones
// that cost a live turn: a busy connection and a remote command that merely
// exited non-zero both look like errors, and dropping the client over either
// tears down every other channel on it.
func TestShouldRedial(t *testing.T) {
	if shouldRedial(nil) {
		t.Error("no error should never earn a re-dial")
	}
	if shouldRedial(fmt.Errorf("%w: too many sessions", errChannelOpen)) {
		t.Error("a channel-open refusal means busy, not dead — must not re-dial")
	}
	if shouldRedial(&ssh.ExitError{}) {
		t.Error("a remote command's exit status means the connection worked — must not re-dial")
	}
	if shouldRedial(fmt.Errorf("wrapped: %w", &ssh.ExitError{})) {
		t.Error("a wrapped exit status must still not re-dial")
	}
	if !shouldRedial(errors.New("connection reset by peer")) {
		t.Error("a transport error must earn a re-dial")
	}
}

// TestDropDefersCloseWhileChannelsInFlight is the anti-regression for the storm:
// evicting a pooled connection must never close the transport out from under a
// turn already streaming on it. The eviction itself is immediate (nobody new gets
// the connection); the close waits for the last in-flight channel.
func TestDropDefersCloseWhileChannelsInFlight(t *testing.T) {
	pool := &SSHPool{entries: map[string]*poolEntry{}}
	closes := 0
	c := &pooledConn{closeConn: func() error { closes++; return nil }}
	e := pool.entry("mom")
	e.mu.Lock()
	e.conns = []*pooledConn{c}
	e.mu.Unlock()

	c.hold() // a turn's channel
	pool.drop("mom", c)

	e.mu.Lock()
	cached := len(e.conns)
	e.mu.Unlock()
	if cached != 0 {
		t.Error("drop left the connection cached; a re-dial would hand it back out")
	}
	if closes != 0 {
		t.Fatal("drop closed the connection with a channel still in flight")
	}
	c.done() // the turn finishes
	if closes != 1 {
		t.Fatalf("closes after the last channel = %d, want 1", closes)
	}
	// A drop of an idle connection closes it straight away, and stays idempotent.
	idle := &pooledConn{closeConn: func() error { closes++; return nil }}
	pool.drop("mom", idle)
	pool.drop("mom", idle)
	if closes != 2 {
		t.Fatalf("closes = %d, want 2 (idle drop closes once; a repeat drop is a no-op)", closes)
	}
}

// A host's other connections must survive one being dropped — the whole point of
// holding several. Before this, evicting a connection cleared the host's single
// cached client, so a probe's transport error took a streaming turn down with it.
func TestDropEvictsOnlyTheOffendingConnection(t *testing.T) {
	pool := &SSHPool{entries: map[string]*poolEntry{}}
	noop := func() error { return nil }
	bad := &pooledConn{closeConn: noop}
	good := &pooledConn{closeConn: noop}
	e := pool.entry("mom")
	e.conns = []*pooledConn{bad, good}

	pool.drop("mom", bad)

	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.conns) != 1 || e.conns[0] != good {
		t.Fatalf("drop left %d connection(s); the host's healthy link must be untouched", len(e.conns))
	}
}

// Channels are spread over a host's connections least-loaded-first, and a retired
// connection is never handed out again.
func TestLeastLoadedSpreadsAndSkipsRetired(t *testing.T) {
	noop := func() error { return nil }
	busy := &pooledConn{closeConn: noop}
	idle := &pooledConn{closeConn: noop}
	busy.chans.tryAcquire(false)
	if got := leastLoaded([]*pooledConn{busy, idle}); got != idle {
		t.Error("a caller must be handed the least-loaded connection")
	}
	idle.retire()
	if got := leastLoaded([]*pooledConn{busy, idle}); got != busy {
		t.Error("a retired connection must never be handed out for new channels")
	}
	if got := leastLoaded(nil); got != nil {
		t.Error("no connections must report none, not panic")
	}
}

// A host that is at its connection cap with every slot taken is the ONE case where
// a caller waits — and that wait must honour ctx rather than hang forever.
func TestAcquireConnWaitsOnlyAtTheConnectionCap(t *testing.T) {
	pool := &SSHPool{entries: map[string]*poolEntry{}}
	e := pool.entry("mom")
	noop := func() error { return nil }
	for i := 0; i < sshMaxConnsPerHost; i++ {
		c := &pooledConn{closeConn: noop}
		for j := 0; j < sshMaxChannels; j++ {
			c.chans.tryAcquire(false)
		}
		e.conns = append(e.conns, c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := pool.acquireConn(ctx, "mom", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("at the cap a caller must queue on ctx, got err = %v", err)
	}
}

// Below the cap, a saturated connection must NOT make the caller wait — the pool
// reaches for another connection. Here the extra dial is guaranteed to fail (the
// host is negative-cached), which proves the dial was attempted, and also that a
// failed bonus dial doesn't steal the error from a host that still has a live link:
// the caller falls back to waiting on it.
func TestAcquireConnDialsAnotherConnectionBeforeWaiting(t *testing.T) {
	pool := &SSHPool{entries: map[string]*poolEntry{}}
	e := pool.entry("mom")
	busy := &pooledConn{closeConn: func() error { return nil }}
	for j := 0; j < sshMaxChannels; j++ {
		busy.chans.tryAcquire(false)
	}
	e.conns = []*pooledConn{busy}
	e.markDown(time.Now(), "mom", errors.New("boom")) // the extra dial will fail fast

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := pool.acquireConn(ctx, "mom", false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a failed extra dial must not fail a caller that could wait on a live link, got %v", err)
	}

	// With no live connection left, the dial failure IS the caller's error.
	pool.drop("mom", busy)
	if _, _, err := pool.acquireConn(context.Background(), "mom", false); err == nil {
		t.Fatal("a host with no connections and a failing dial must return the dial error")
	}
}

// tryAcquire is what turns "this link is full" into "dial another" instead of
// "wait": it must never block, and must not leak a stream slot when the general
// budget is the thing that's exhausted.
func TestTryAcquireNeverBlocksAndUnwindsCleanly(t *testing.T) {
	var b channelBudget
	for i := 0; i < sshMaxChannels; i++ {
		if !b.tryAcquire(false) {
			t.Fatalf("slot %d should have been free", i)
		}
	}
	if b.tryAcquire(false) {
		t.Fatal("a full budget must refuse rather than block")
	}
	if b.tryAcquire(true) {
		t.Fatal("a full budget must refuse a long-lived caller too")
	}
	if len(b.streams) != 0 {
		t.Fatal("a refused long-lived caller must give its stream slot back")
	}
	if b.load() != sshMaxChannels {
		t.Fatalf("load = %d, want %d", b.load(), sshMaxChannels)
	}
}

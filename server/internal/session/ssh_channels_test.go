package session

import (
	"errors"
	"fmt"
	"testing"

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
	e.client = c
	e.mu.Unlock()

	c.hold() // a turn's channel
	pool.drop("mom", c)

	e.mu.Lock()
	cached := e.client
	e.mu.Unlock()
	if cached != nil {
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

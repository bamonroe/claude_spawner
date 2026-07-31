package gateway

import (
	"context"
	"testing"
	"time"
)

// A server with no jobs, or only idle jobs, is immediately idle: WaitForInflight
// returns true without blocking.
func TestWaitForInflightIdle(t *testing.T) {
	s := &Server{jobs: map[string]*sessionJob{}}
	if !s.WaitForInflight(context.Background()) {
		t.Fatal("empty server should be idle")
	}

	s.jobs["a"] = &sessionJob{} // present but not running
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !s.WaitForInflight(ctx) {
		t.Fatal("server with only idle jobs should be idle")
	}
}

// A running turn keeps the server busy until it finishes; WaitForInflight blocks
// and then returns true once the job goes idle within the grace window.
func TestWaitForInflightWaitsThenIdle(t *testing.T) {
	j := &sessionJob{running: true}
	s := &Server{jobs: map[string]*sessionJob{"a": j}}

	// Flip the job idle shortly after the wait begins; the wait should observe it.
	go func() {
		time.Sleep(150 * time.Millisecond)
		j.mu.Lock()
		j.running = false
		j.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if !s.WaitForInflight(ctx) {
		t.Fatal("wait should have observed the job going idle")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("wait returned too early (%v); it should have blocked until the turn finished", elapsed)
	}
}

// If the grace window expires with a turn still running, WaitForInflight reports
// the server is NOT idle so shutdown falls through to interrupting it.
func TestWaitForInflightExpiresWhileBusy(t *testing.T) {
	j := &sessionJob{running: true}
	s := &Server{jobs: map[string]*sessionJob{"a": j}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if s.WaitForInflight(ctx) {
		t.Fatal("wait should report busy when the grace window expires with a turn still running")
	}
}

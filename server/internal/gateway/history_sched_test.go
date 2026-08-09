package gateway

import (
	"testing"
	"time"
)

// TestHistoryGateReservesForegroundCapacity is the starvation case the two-lane
// scheduler exists for: N background prefetch reads are parked (blocked on a
// slow host and never releasing), and a foreground request must still be
// admitted promptly rather than queueing behind them.
func TestHistoryGateReservesForegroundCapacity(t *testing.T) {
	g := newHistoryGate(historyConcurrency, historyBackgroundConcurrency)

	// Saturate the background lane and leave it saturated.
	for i := 0; i < historyBackgroundConcurrency; i++ {
		g.acquire(true)
	}
	// More prefetches pile up behind the lane cap and stay blocked.
	for i := 0; i < 8; i++ {
		go g.acquire(true)
	}
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		g.acquire(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("foreground request starved behind parked background reads")
	}
}

// TestHistoryGateWakesForegroundFirst checks the second rule: when a slot frees
// and both lanes have waiters, the foreground one is admitted first.
func TestHistoryGateWakesForegroundFirst(t *testing.T) {
	g := newHistoryGate(1, 1)
	g.acquire(true) // the single slot is held by a background read

	bg := make(chan struct{})
	go func() { g.acquire(true); close(bg) }()
	time.Sleep(20 * time.Millisecond) // ensure the background waiter queued first

	fg := make(chan struct{})
	go func() { g.acquire(false); close(fg) }()
	time.Sleep(20 * time.Millisecond)

	g.release(true)
	select {
	case <-fg:
	case <-bg:
		t.Fatal("background waiter admitted ahead of a waiting foreground request")
	case <-time.After(2 * time.Second):
		t.Fatal("nobody admitted after release")
	}
}

// TestHistoryGateReleaseHandsOffSlots walks a full round trip to make sure the
// accounting balances: every waiter eventually runs and the gate ends empty.
func TestHistoryGateReleaseHandsOffSlots(t *testing.T) {
	g := newHistoryGate(2, 1)
	ran := make(chan bool, 6)
	for i := 0; i < 3; i++ {
		go func() { g.acquire(true); ran <- true; g.release(true) }()
		go func() { g.acquire(false); ran <- false; g.release(false) }()
	}
	for i := 0; i < 6; i++ {
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 6 requests ran", i)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.used != 0 || g.bgUsed != 0 || len(g.fgWait) != 0 || len(g.bgWait) != 0 {
		t.Fatalf("gate not drained: used=%d bgUsed=%d fgWait=%d bgWait=%d", g.used, g.bgUsed, len(g.fgWait), len(g.bgWait))
	}
}

package gateway

import "sync"

// historyGate is the two-lane admission scheduler for a connection's history
// reads. It exists because one flat semaphore let speculative prefetch traffic
// starve the session the user is actually looking at: every slot could be held
// by a background read that is blocked on a remote probe, and the foreground
// request then waited behind all of them.
//
// Two rules make the foreground lane immune to that:
//
//  1. **Reserved capacity.** At most bgCap of the cap slots may be held by
//     background reads, so a foreground request always has somewhere to land no
//     matter how many prefetches are parked on a dead host.
//  2. **Foreground-first wakeup.** When a slot frees, waiting foreground
//     requests are admitted before any waiting background one, FIFO within each
//     lane.
type historyGate struct {
	mu     sync.Mutex
	cap    int // total reads in flight
	bgCap  int // of those, how many may be background
	used   int
	bgUsed int
	fgWait []chan struct{} // FIFO of waiters, one channel each
	bgWait []chan struct{}
}

func newHistoryGate(capacity, bgCapacity int) *historyGate {
	return &historyGate{cap: capacity, bgCap: bgCapacity}
}

// acquire blocks until this request may run. Callers must release exactly once
// with the same background flag.
func (g *historyGate) acquire(background bool) {
	g.mu.Lock()
	if g.admit(background) {
		g.mu.Unlock()
		return
	}
	ch := make(chan struct{})
	if background {
		g.bgWait = append(g.bgWait, ch)
	} else {
		g.fgWait = append(g.fgWait, ch)
	}
	g.mu.Unlock()
	<-ch // the releasing goroutine charged our slot before signalling
}

// admit takes a slot if one is free for this lane. Caller holds g.mu.
func (g *historyGate) admit(background bool) bool {
	if g.used >= g.cap {
		return false
	}
	if background {
		if g.bgUsed >= g.bgCap {
			return false
		}
		g.bgUsed++
	}
	g.used++
	return true
}

func (g *historyGate) release(background bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.used--
	if background {
		g.bgUsed--
	}
	// Hand the freed slot straight to the next waiter, charging it here so no
	// third party can slip in between the signal and the waiter waking.
	if len(g.fgWait) > 0 {
		ch := g.fgWait[0]
		g.fgWait = g.fgWait[1:]
		g.used++
		close(ch)
		return
	}
	if len(g.bgWait) > 0 && g.bgUsed < g.bgCap {
		ch := g.bgWait[0]
		g.bgWait = g.bgWait[1:]
		g.used++
		g.bgUsed++
		close(ch)
	}
}

package gateway

import (
	"sync"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// Attach resolution may need the sessions that exist ON DISK but aren't in the
// registry yet — a session started by an interactive `claude`, or one this
// server has never adopted. Finding them means walking ~/.claude/projects, which
// on a cold server is seconds of remote I/O, and attach runs on the connection's
// serial read loop: done inline, one unlucky attach stalls every message queued
// behind it (the app's history request first).
//
// attachDiscovery makes that walk cost bounded instead of unbounded. The result
// is memoized for attachDiscoveryTTL and refreshed by a single background walk
// (concurrent callers join the one in flight); a caller waits at most
// attachDiscoveryWait for it and otherwise proceeds with whatever it already
// has. So the worst case on the read loop is a fixed fraction of a second, the
// walk still completes and warms the memo, and the client's retry — which it
// makes because the attach is now always nacked rather than dropped — resolves
// instantly against it.
const (
	attachDiscoveryTTL  = 15 * time.Second
	attachDiscoveryWait = 750 * time.Millisecond
)

type attachDiscovery struct {
	mu    sync.Mutex
	list  []session.Discovered
	at    time.Time     // when list was last refreshed; zero = never
	ready chan struct{} // non-nil while a walk is in flight, closed when it lands
}

// discoverForAttach returns the on-disk sessions attach resolution should match
// against. Never blocks longer than attachDiscoveryWait, and never runs more
// than one walk at a time. A cold first call may return nothing — the caller
// nacks, the walk lands, and the retry hits the memo.
func (s *Server) discoverForAttach() []session.Discovered {
	d := &s.discoverMemo
	d.mu.Lock()
	if !d.at.IsZero() && time.Since(d.at) < attachDiscoveryTTL {
		list := d.list
		d.mu.Unlock()
		return list
	}
	wait := d.ready
	if wait == nil {
		wait = make(chan struct{})
		d.ready = wait
		go s.refreshAttachDiscovery(wait)
	}
	stale := d.list
	d.mu.Unlock()

	t := time.NewTimer(attachDiscoveryWait)
	defer t.Stop()
	select {
	case <-wait:
		d.mu.Lock()
		list := d.list
		d.mu.Unlock()
		return list
	case <-t.C:
		return stale // the walk keeps going; the next call gets its result
	}
}

// discoverSnapshot is discoverForAttach plus whether the memo holds ANY
// successful walk. ok=false means the walk has never landed (cold server, or a
// scan failure before the first success) — the caller has no on-disk view at
// all and should say so (`partial`) rather than fail or pretend the disk is
// empty. A stale-but-successful memo still reports ok=true: stale rows beat no
// rows, and the refresh that's already in flight will feed the next request.
func (s *Server) discoverSnapshot() ([]session.Discovered, bool) {
	list := s.discoverForAttach()
	d := &s.discoverMemo
	d.mu.Lock()
	ok := !d.at.IsZero()
	d.mu.Unlock()
	return list, ok
}

// refreshAttachDiscovery performs the one in-flight walk and publishes it. A
// failed walk still clears `ready` (so the next caller can retry) but leaves the
// previous list and its timestamp alone, so a transient host outage doesn't
// throw away a good memo.
func (s *Server) refreshAttachDiscovery(done chan struct{}) {
	found, err := s.driver.DiscoverSessions("")
	d := &s.discoverMemo
	d.mu.Lock()
	if err == nil {
		d.list, d.at = found, time.Now()
	}
	d.ready = nil
	d.mu.Unlock()
	close(done)
}

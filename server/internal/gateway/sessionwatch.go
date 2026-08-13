package gateway

import (
	"context"
	"time"
)

// The server-driven session-list push. `discovered` used to be response-only: a
// client learned about a spawn, rename, adopt or delete only if it had asked, so
// a session created on the phone sat invisible in the desktop's sidebar (and
// radial menu) until a manual refresh. The store now reports list-visible
// mutations (session.Store.Subscribe), and this watcher turns each one into an
// unsolicited `discovered` frame to every connected client — the same shape
// hosts.go has always used for host_list.
//
// The client side needed nothing: both surfaces collect the one `discovered`
// StateFlow the controller assigns from any incoming frame, solicited or not.

// sessionPushDebounce is the coalescing window. One logical change touches the
// store more than once (a spawn registers the record, then stamps it), so the
// watcher waits this long after the first wake and pushes the settled list once
// rather than a frame per write. Short enough to feel instant on the other
// device, long enough to swallow a burst.
const sessionPushDebounce = 250 * time.Millisecond

// watchSessionList subscribes to the store and starts the debounced broadcaster.
// The subscription callback runs on the mutating goroutine, so it does nothing
// but a non-blocking wake — all the work happens on the watcher's own goroutine.
func (s *Server) watchSessionList() {
	wake := make(chan struct{}, 1)
	s.store.Subscribe(func() {
		select {
		case wake <- struct{}{}:
		default: // a push is already pending; it will carry this change too
		}
	})
	go s.sessionListPushLoop(wake)
}

func (s *Server) sessionListPushLoop(wake <-chan struct{}) {
	for range wake {
		time.Sleep(sessionPushDebounce)
		// Absorb the rest of the burst: anything that landed during the window is
		// already in the list we're about to build. A change that arrives after this
		// drain re-arms the channel and gets its own push.
		select {
		case <-wake:
		default:
		}
		s.pushSessionList()
	}
}

// pushSessionList broadcasts the current session list to every connected client.
// It reads the memoized walk only (never triggering one), so the rows are exactly
// what a `discover` request would answer from cache; if no walk has ever landed
// the frame is flagged partial, which the client treats as do-not-prune.
func (s *Server) pushSessionList() {
	conns := s.connSnapshot()
	if len(conns) == 0 {
		return // nobody to tell; the next client to connect asks for the list itself
	}
	found, ok := s.discoverCachedSnapshot()
	msg := msgDiscovered(s.discoveredViews(context.Background(), found), !ok)
	for _, c := range conns {
		c.post(msg)
	}
}

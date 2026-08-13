package session

import (
	"slices"
	"sort"
	"sync"
)

// The store's change-notification hook. Every mutation site used to write
// straight to the Store and tell nobody, so a session spawned/renamed/killed on
// one connection left every other client's sidebar and radial menu stale until a
// manual refresh. Rather than sprinkling a broadcast call at each callsite (and
// missing the next one added), the notification lives inside the Store: any
// present or future mutation is covered by construction.
//
// The signal is deliberately field-aware. Put is called on EVERY turn (turns.go,
// dictate.go) and almost none of those writes change anything the list renders,
// so the store compares the list projection before and after and only fires when
// it actually differs — the broadcast layer is not woken once per turn.

// listRow is the projection of a record that the client-facing session list
// renders: the fields sessionView/discoveredView read out of the store. A change
// to anything else (Jobs, PendingNotes, AskPrimed, transcript bookkeeping) is
// invisible to the list and must not wake subscribers. `busy` is list-visible too
// but is NOT a store field — the gateway derives it from live turn state and
// signals that itself.
type listRow struct {
	Name      string
	Dir       string
	SessionID string
	Host      string
	Target    Target
	Agent     string
	Model     string
	Profile   string
}

// watch is the Store's subscriber registry plus the last list projection it
// broadcast, guarded by its own mutex so notifying never touches the store lock.
type watch struct {
	mu   sync.Mutex
	next uint64
	subs map[uint64]func()
	rows []listRow // the projection subscribers were last told about
}

// Subscribe registers fn to be called after any mutation that changes the
// list-visible shape of the registry (a record added, removed, renamed, or one
// of its listRow fields rewritten). It returns a cancel func that unsubscribes.
//
// fn runs on the mutating goroutine with NO store lock held, but it still sits
// on the mutator's path: it must not block or call back into the Store. The
// intended body is a non-blocking wake of a debounced broadcaster.
func (s *Store) Subscribe(fn func()) (cancel func()) {
	s.watch.mu.Lock()
	defer s.watch.mu.Unlock()
	if s.watch.subs == nil {
		s.watch.subs = map[uint64]func(){}
	}
	id := s.watch.next
	s.watch.next++
	s.watch.subs[id] = fn
	return func() {
		s.watch.mu.Lock()
		delete(s.watch.subs, id)
		s.watch.mu.Unlock()
	}
}

// listRows snapshots the current list projection, sorted so the comparison is
// order-independent (the store is map-backed). Each record's fields are read
// under ITS own lock — the map lock says nothing about them — and no store lock
// is held while comparing or calling out.
func (s *Store) listRows() []listRow {
	s.mu.RLock()
	recs := make([]*Session, 0, len(s.byName))
	for _, rec := range s.byName {
		recs = append(recs, rec)
	}
	s.mu.RUnlock()
	rows := make([]listRow, 0, len(recs))
	for _, rec := range recs {
		r := rec.Snapshot()
		rows = append(rows, listRow{
			Name: r.Name, Dir: r.Dir, SessionID: r.SessionID, Host: r.Host,
			Target: r.Target, Agent: r.Agent, Model: r.Model, Profile: r.Profile,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Name != rows[j].Name {
			return rows[i].Name < rows[j].Name
		}
		return rows[i].SessionID < rows[j].SessionID
	})
	return rows
}

// seedWatch records the projection the store loaded with, so the first mutation
// compares against reality rather than against "empty" and fires spuriously.
func (s *Store) seedWatch() {
	rows := s.listRows()
	s.watch.mu.Lock()
	s.watch.rows = rows
	s.watch.mu.Unlock()
}

// notify fires subscribers if — and only if — the list projection changed since
// the last time they were told. Call it from a mutation AFTER releasing the
// store lock. The compare-and-swap of the remembered projection happens under
// watch.mu, so two concurrent mutations produce exactly one notification each of
// the states they land on, never a lost or duplicated wake.
func (s *Store) notify() {
	rows := s.listRows()
	s.watch.mu.Lock()
	if slices.Equal(s.watch.rows, rows) {
		s.watch.mu.Unlock()
		return
	}
	s.watch.rows = rows
	fns := make([]func(), 0, len(s.watch.subs))
	for _, fn := range s.watch.subs {
		fns = append(fns, fn)
	}
	s.watch.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

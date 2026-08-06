package session

import "sync"

// displayMemo memoizes assembled display histories, keyed by the freshness
// signature (path+size+mtime per transcript) they were read at.
//
// It exists because a digest MISS used to mean re-parsing a session's whole
// cross-backend chat log from scratch: every archived HistorySegment plus the
// current chain, hundreds of ms to seconds over SSH — and every history page-back
// (before != nil) skips the digest fast path entirely, so paging older messages
// paid that cost per page. Two levels of memo remove both:
//
//   - whole: the assembled, re-indexed log for a session at one signature. A hit
//     serves paging and repeat fetches with no reads at all.
//   - segments: one archived segment's messages, keyed by its own signature.
//     Archived segments belong to a backend the session has switched away from, so
//     their signature never changes again — a session that keeps producing output
//     misses `whole` on every turn but still never re-reads its archives.
//
// Both levels hand out COPIES: callers mutate the messages they get (serveHistory
// strips injected scaffolding in place, over a slice that aliases the page), and a
// memo that shared its backing array would hand the next caller stripped text.
//
// Safe for concurrent use. Purely a speed cache: a miss costs a read, never
// correctness, and an empty signature (a backend that can't describe its chain
// cheaply) is never stored.
type displayMemo struct {
	mu       sync.Mutex
	whole    map[string]memoEntry // session_id → the log, and the sig it was read at
	segments map[string][]Message // "<agent>@<sig>" → one archived segment's messages
}

type memoEntry struct {
	sig  string
	msgs []Message
}

// memoSegmentCap bounds the archived-segment map. Segments are immutable once
// archived, so entries never go stale — the cap only keeps a long-lived server
// that has switched backends many times from growing without bound. Well past any
// realistic working set; overflowing just clears and re-warms.
const memoSegmentCap = 256

func newDisplayMemo() *displayMemo {
	return &displayMemo{whole: map[string]memoEntry{}, segments: map[string][]Message{}}
}

// getWhole returns the assembled log for a session when it was read at exactly
// this signature. An empty sig is never a hit.
func (m *displayMemo) getWhole(sessionID, sig string) ([]Message, bool) {
	if m == nil || sig == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.whole[sessionID]
	if !ok || e.sig != sig {
		return nil, false
	}
	return append([]Message(nil), e.msgs...), true
}

func (m *displayMemo) putWhole(sessionID, sig string, msgs []Message) {
	if m == nil || sig == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.whole[sessionID] = memoEntry{sig: sig, msgs: append([]Message(nil), msgs...)}
}

func (m *displayMemo) getSegment(key string) ([]Message, bool) {
	if m == nil || key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.segments[key]
	if !ok {
		return nil, false
	}
	return append([]Message(nil), msgs...), true
}

func (m *displayMemo) putSegment(key string, msgs []Message) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.segments) >= memoSegmentCap {
		m.segments = map[string][]Message{}
	}
	m.segments[key] = append([]Message(nil), msgs...)
}

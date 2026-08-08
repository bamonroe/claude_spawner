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
//   - prefix: all archived segments already concatenated AND indexed, keyed on the
//     joined segment signatures. The archived part of a log is a stable prefix; only
//     the current chain moves. Caching the joined prefix means a turn re-reads and
//     re-indexes only the current chain instead of walking the whole log, so
//     per-turn assembly is O(current chain) even when the archive is huge.
//
// Both levels hand out READ-ONLY slices, shared with the stored entry rather than
// copied. Copying was once needed because serveHistory stripped injected
// scaffolding in place, over a slice aliasing the memo's array — but that made
// even a HIT cost an O(total-messages) copy on every get and put. The mutation now
// happens on a fresh copy of the requested PAGE (gateway.serveHistory), so the memo
// hands out its arrays directly: callers MUST NOT mutate the messages they get.
//
// Safe for concurrent use. Purely a speed cache: a miss costs a read, never
// correctness, and an empty signature (a backend that can't describe its chain
// cheaply) is never stored.
type displayMemo struct {
	mu       sync.Mutex
	whole    map[string]memoEntry // session_id → the log, and the sig it was read at
	segments map[string][]Message // "<agent>@<sig>" → one archived segment's messages
	prefix   map[string][]Message // joined segment sigs → every archived segment, concatenated and indexed
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
	return &displayMemo{whole: map[string]memoEntry{}, segments: map[string][]Message{}, prefix: map[string][]Message{}}
}

// getWhole returns the assembled log for a session when it was read at exactly
// this signature. An empty sig is never a hit. The result is read-only.
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
	return e.msgs, true
}

// putWhole stores msgs by reference: the caller must not mutate it afterwards.
func (m *displayMemo) putWhole(sessionID, sig string, msgs []Message) {
	if m == nil || sig == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.whole[sessionID] = memoEntry{sig: sig, msgs: msgs}
}

// getSegment returns one archived segment's messages. The result is read-only.
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
	return msgs, true
}

// putSegment stores msgs by reference: the caller must not mutate it afterwards.
func (m *displayMemo) putSegment(key string, msgs []Message) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.segments) >= memoSegmentCap {
		m.segments = map[string][]Message{}
	}
	m.segments[key] = msgs
}

// getPrefix returns the concatenated, already-indexed archived prefix for a joined
// segment signature. The result is read-only.
func (m *displayMemo) getPrefix(key string) ([]Message, bool) {
	if m == nil || key == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	msgs, ok := m.prefix[key]
	return msgs, ok
}

// putPrefix stores msgs by reference: the caller must not mutate it afterwards.
func (m *displayMemo) putPrefix(key string, msgs []Message) {
	if m == nil || key == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.prefix) >= memoSegmentCap {
		m.prefix = map[string][]Message{}
	}
	m.prefix[key] = msgs
}

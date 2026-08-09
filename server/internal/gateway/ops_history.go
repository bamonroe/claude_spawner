package gateway

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// historyConcurrency bounds how many history reads one connection runs at once.
// History is served OFF the inbound loop (see startHistory), so a burst of
// prefetch requests could otherwise open one transcript read per request; a
// small pool keeps the disk/SSH fan-out bounded while still letting a switch's
// history overlap an attach.
const historyConcurrency = 4

// historyBackgroundConcurrency caps how many of those slots speculative prefetch
// reads may occupy. The remainder is reserved for foreground (user-visible)
// requests, so a burst of prefetches against a slow host — each able to sit on
// the 30s remote probe timeout — can never leave the session the user is looking
// at with nowhere to run. See historyGate.
const historyBackgroundConcurrency = 2

// historyReq is one queued history request; see startHistory.
type historyReq struct {
	sessionID, name string
	before          *int
	limit           int
	haveHash        string
	havePrefix      string
	havePrefixCount int
	background      bool // speculative prefetch: runs in the deprioritised lane
}

// historySlot is the per-session coalescing record: which lane the in-flight
// top-page read is running in, and the latest request parked behind it.
type historySlot struct {
	background bool
	parked     *historyReq
}

// startHistory serves a history request off the connection's serial inbound
// loop. Inbound messages dispatch one at a time, so serving history inline made
// a switch's history request queue behind attach's transcript stats — and any
// concurrent discover/browse/turn blocked both. The read itself is safe off the
// loop: it touches only the store and the driver (both internally locked), and
// replies serialize through wmu like every other cross-goroutine send.
//
// Top-page requests (before == nil) coalesce per session: while one is in
// flight, later arrivals for the same session don't spawn their own reads —
// the LATEST one is parked and runs when the in-flight read finishes (earlier
// parked ones are superseded; top-page requests only differ by have_hash, and
// the newest is the client's current state). Parking rather than dropping
// matters: the in-flight read may have already sent its reply by the time the
// next request arrives, and a dropped request would then never be answered.
// A burst of N prefetch requests thus costs at most two reads, and can't
// stampede the same transcript. Paging requests (before != nil) always run;
// their cursors differ, so each reply is distinct.
func (c *conn) startHistory(sessionID, name string, before *int, limit int, haveHash, havePrefix string, havePrefixCount int, background bool) {
	c.historyMu.Lock()
	if c.historyGate == nil {
		c.historyGate = newHistoryGate(historyConcurrency, historyBackgroundConcurrency)
		c.historyBusy = make(map[string]*historySlot)
	}
	gate := c.historyGate
	req := historyReq{sessionID: sessionID, name: name, before: before, limit: limit, haveHash: haveHash,
		havePrefix: havePrefix, havePrefixCount: havePrefixCount, background: background}
	if before != nil {
		c.historyMu.Unlock()
		go func() {
			gate.acquire(req.background)
			defer gate.release(req.background)
			c.serveHistory(req)
		}()
		return
	}
	key := sessionID
	if key == "" {
		key = name
	}
	if slot, busy := c.historyBusy[key]; busy {
		// Coalescing must never make a foreground request wait on a background
		// one: the in-flight prefetch may be parked on a slow host for tens of
		// seconds, and queueing behind it would hand the user exactly the stall
		// the priority lane exists to prevent. So a foreground arrival that finds
		// a background read in flight runs its own read in the reserved lane;
		// same-lane bursts still coalesce as before.
		if !req.background && slot.background {
			c.historyMu.Unlock()
			go func() {
				gate.acquire(false)
				defer gate.release(false)
				c.serveHistory(req)
			}()
			return
		}
		slot.parked = &req // supersede any earlier parked request
		c.historyMu.Unlock()
		return
	}
	slot := &historySlot{background: req.background}
	c.historyBusy[key] = slot
	c.historyMu.Unlock()
	go func() {
		for {
			gate.acquire(req.background)
			c.serveHistory(req)
			gate.release(req.background)
			c.historyMu.Lock()
			parked := slot.parked
			if parked == nil {
				delete(c.historyBusy, key)
				c.historyMu.Unlock()
				return
			}
			slot.parked = nil
			slot.background = parked.background
			c.historyMu.Unlock()
			req = *parked
		}
	}()
}

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.
// The target is addressed by the stable `session_id` when the client sends one —
// the same handle every other session op uses. A name is only a fallback (older
// clients): names are per-server and get renamed on another device, so keying
// history by name made a renamed session answer `no_session` and silently never
// refresh. GetByAnyID also accepts an id retired by a clear/compress rotation, so
// a client holding a pre-rotation id still reaches the live record.
func (c *conn) serveHistory(req historyReq) {
	sessionID, name, before, limit, haveHash := req.sessionID, req.name, req.before, req.limit, req.haveHash
	var s *session.Session
	if sessionID != "" {
		s = c.srv.store.GetByAnyID(sessionID)
	}
	if s == nil {
		s = c.srv.store.Get(name)
	}
	if s == nil {
		c.fail("no_session", "no such session: "+name)
		return
	}
	name = recName(s) // the reply echoes the session's current name, not the client's stale one
	// Cache-validation fast path: a top-page request (before == nil) carrying the
	// hash the app already holds needs no message bodies — tell it the cache is
	// current so clicking back into an unchanged session transfers nothing.
	//
	// This runs BEFORE the full read, via DisplayDigest, which answers from a few
	// stats when the chain hasn't moved. Doing the read first made this "fast path"
	// no faster than a miss: every attach parsed the session's whole transcript
	// chain (tens of megabytes, over SSH) only to discard it — and because the
	// gateway dispatches serially, it blocked every other request on the connection.
	if before == nil && haveHash != "" {
		if count, hash, _, err := c.srv.driver.DisplayDigest(c.ctx, s); err == nil && hash == haveHash {
			c.send(msgHistory(recID(s), name, nil, false, count, hash, true, historyDelta{}))
			return
		}
	}
	// One call for bodies AND digest: it stats the chain once for both, serves the
	// log from the display memo when nothing has moved (which is every page-back —
	// `before != nil` skips the fast path above, so paging older messages used to
	// re-parse the entire chain per page), and re-reads only the archived segments
	// that actually changed, which for an archive is never.
	msgs, count, hash, err := c.srv.driver.DisplayHistory(c.ctx, s)
	if err != nil {
		c.fail("history_failed", err.Error())
		return
	}
	// Delta path: the app echoed back a prefix digest this server issued earlier.
	// If it still describes our own first have_prefix_count display rows, the app's
	// cache is a valid PREFIX of the current log — everything it holds is correct,
	// it's just missing the tail. Send only the appended rows instead of a whole
	// page, which is the common shape after a turn lands (a couple of new rows on
	// top of a long log). Anchoring on (count, prefix hash) rather than
	// Message.Index is what makes this safe across a clear/compress rotation: a
	// rotation rewrites every Index but the ID-keyed digest survives it.
	if before == nil && req.havePrefix != "" && req.havePrefixCount <= len(msgs) {
		if _, h := session.HistoryPrefixDigest(msgs, req.havePrefixCount); h == req.havePrefix {
			tail := msgs[req.havePrefixCount:]
			// A tail longer than a page means the app has been away a while; fall
			// back to a normal page rather than shipping an unbounded delta.
			if max := pageLimit(limit); len(tail) <= max {
				_, ph := session.HistoryPrefixDigest(msgs, len(msgs))
				c.send(msgHistory(recID(s), name, stripPage(tail), false, count, hash, false,
					historyDelta{PrefixHash: ph, PrefixCount: len(msgs), Delta: true, BaseCount: req.havePrefixCount}))
				return
			}
		}
	}
	b := -1
	if before != nil {
		b = *before
	}
	page, more := session.HistoryPage(msgs, b, limit)
	// The prefix digest covers the log through the last row of THIS page, so the
	// app can echo it back next time (it never computes the hash itself). For a
	// top page that's the whole log; for a page-back it's the cursor.
	pageEnd := len(msgs)
	if b >= 0 && b < pageEnd {
		pageEnd = b
	}
	_, prefixHash := session.HistoryPrefixDigest(msgs, pageEnd)
	// Strip the server-injected scaffolding from user messages so replayed history
	// matches what the live view showed (and never re-surfaces hidden instructions).
	// A user row that is ENTIRELY scaffolding (the autonomous job-notify turn) strips
	// to empty — drop it so history shows no bubble the live view never did. The
	// digest (count/hash) is over the full stored transcript, not this display page,
	// so omitting a display row doesn't disturb the app's freshness check or paging
	// (each kept row carries its own transcript index).
	c.send(msgHistory(recID(s), name, stripPage(page), more, count, hash, false,
		historyDelta{PrefixHash: prefixHash, PrefixCount: pageEnd}))
}

// pageLimit is HistoryPage's page-size defaulting, shared so the delta tail cap
// and the page it falls back to can't disagree about how big a page is.
func pageLimit(limit int) int {
	if limit <= 0 {
		return 30
	}
	return limit
}

// stripPage applies the scaffolding strip to one display page, returning a NEW
// slice: DisplayHistory hands back a read-only log (a memo hit aliases the cache),
// so rewriting Text in place would serve stripped text to every later reader. The
// copy is O(page), which is what lets the memo skip its O(whole log) copy per hit.
func stripPage(page []session.Message) []session.Message {
	kept := make([]session.Message, 0, len(page))
	for i := range page {
		m := page[i]
		if m.Role == "user" {
			m.Text = stripInjected(m.Text)
			if strings.TrimSpace(m.Text) == "" {
				continue
			}
		}
		kept = append(kept, m)
	}
	return kept
}

// serveDigests reports every registered session's transcript digest (message
// count + content hash) so the app can validate its offline transcript cache on
// connect without transferring any message bodies — it refetches history only
// for sessions whose hash changed. Transcript reads are memoized by file stat,
// so recomputing digests when nothing changed is cheap. An unreadable session is
// skipped (the app keeps whatever it already cached for it).

// digestSweepConcurrency bounds how many sessions the sweep reads at once. The
// reads are independent and mostly wait on remote I/O, so overlapping them turns
// a sum of latencies into roughly the slowest one.
//
// It is no longer this constant's job to stay under the peer's SSH channel
// ceiling — the pool budgets channels per connection now (see
// session/ssh_channels.go), so a sweep that outruns the budget queues instead of
// getting the whole pooled client wrongly dropped as stale. This is purely how
// much work the sweep tries to have in flight.
const digestSweepConcurrency = 8

// digestSessionDeadline caps how long ONE session's digest may take before the
// sweep gives up on it and reports the rest. Without it the frame was held until
// every session answered, so a single slow or half-dead host delayed the app's
// whole connect-time cache validation — and therefore its history refresh — by
// minutes. A session that misses the deadline is simply omitted from the frame,
// which the app already treats as "keep the cached transcript" (the same thing an
// unreadable session has always meant), and the next sweep tries it again.
//
// It is generous on purpose: a cold chain on a healthy remote host is a handful
// of stats plus, on a digest miss, a full parse. This is the "this host is not
// answering" threshold, not a latency target.
const digestSessionDeadline = 8 * time.Second

// startDigestSweep runs the sweep OFF the connection's inbound loop. The loop
// handles one message at a time, so doing this inline made every other request —
// attach, history, anything the user tapped — wait behind a full walk of every
// session's transcripts. On a cold server that walk is the expensive one (nothing
// is memoized yet), which is exactly when the user is reconnecting and least able
// to wait.
//
// Only one sweep runs per connection at a time: a request arriving while one is
// in flight is dropped rather than queued, because the running sweep sends a
// `digests` message that answers it too. That also stops a client that re-requests
// on a timer from stacking sweeps.
func (c *conn) startDigestSweep() {
	if !c.digestSweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.digestSweeping.Store(false)
		c.serveDigests()
	}()
}

// serveDigests computes and sends the sweep. Callers should go through
// startDigestSweep rather than calling this on the inbound loop.
func (c *conn) serveDigests() {
	sessions := c.srv.store.List()
	// Indexed writes (not appends) so the result keeps the store's order
	// regardless of which read finishes first.
	items := make([]digestView, len(sessions))
	served := make([]bool, len(sessions))
	sem := make(chan struct{}, digestSweepConcurrency)
	var wg sync.WaitGroup
	var hits, timedOut atomic.Int64
	started := time.Now()
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s *session.Session) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// Per-session deadline: the read's own context, not a wrapper around a
			// blocking call — the timeout reaches the SSH probes themselves, so a
			// dead host's session releases its slot instead of pinning one for the
			// full probe timeout while the other sessions wait behind it.
			ctx, cancel := context.WithTimeout(c.ctx, digestSessionDeadline)
			defer cancel()
			// DisplayDigest, not ReadDisplayHistory: when the session's transcripts
			// haven't moved this is a few stats instead of a full parse.
			count, hash, cached, err := c.srv.driver.DisplayDigest(ctx, s)
			if err != nil {
				// Unreadable, or out of time: either way the app keeps whatever it
				// already cached for this session and the next sweep retries it.
				if ctx.Err() != nil {
					timedOut.Add(1)
				}
				return
			}
			if cached {
				hits.Add(1)
			}
			snap := s.Snapshot()
			items[i] = digestView{Name: snap.Name, SessionID: snap.SessionID, Count: count, Hash: hash}
			served[i] = true
		}(i, s)
	}
	wg.Wait()
	out := make([]digestView, 0, len(items))
	for i, ok := range served {
		if ok {
			out = append(out, items[i])
		}
	}
	// Logged because this sweep is the one operation whose cost is invisible to the
	// user (it runs in the background) yet dominates a cold connect: the hit count
	// is how you tell a working digest cache from one that is silently missing.
	log.Printf("digest sweep: %d session(s), %d cached, %d recomputed, %d timed out in %v",
		len(sessions), hits.Load(), int64(len(out))-hits.Load(), timedOut.Load(), time.Since(started).Round(time.Millisecond))
	c.send(msgDigests(out))
}

// commandHelp is spoken + shown when the user asks "hey buddy help".
const commandHelp = "here's what I know: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, clear the context, compress the context, " +
	"list models, use model by number, set the target or directory, stop the turn, cancel message, and help. " +
	"say hey buddy, then the command, then your end token."

	// sandboxTarget returns the session's target string only when it's a sandbox
	// session (the non-default target the app badges); "" for host sessions.

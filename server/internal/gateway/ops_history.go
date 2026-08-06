package gateway

import (
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.
// The target is addressed by the stable `session_id` when the client sends one —
// the same handle every other session op uses. A name is only a fallback (older
// clients): names are per-server and get renamed on another device, so keying
// history by name made a renamed session answer `no_session` and silently never
// refresh. GetByAnyID also accepts an id retired by a clear/compress rotation, so
// a client holding a pre-rotation id still reaches the live record.
func (c *conn) serveHistory(sessionID, name string, before *int, limit int, haveHash string) {
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
		if count, hash, _, err := c.srv.driver.DisplayDigest(s); err == nil && hash == haveHash {
			c.send(msgHistory(recID(s), name, nil, false, count, hash, true))
			return
		}
	}
	// One call for bodies AND digest: it stats the chain once for both, serves the
	// log from the display memo when nothing has moved (which is every page-back —
	// `before != nil` skips the fast path above, so paging older messages used to
	// re-parse the entire chain per page), and re-reads only the archived segments
	// that actually changed, which for an archive is never.
	msgs, count, hash, err := c.srv.driver.DisplayHistory(s)
	if err != nil {
		c.fail("history_failed", err.Error())
		return
	}
	b := -1
	if before != nil {
		b = *before
	}
	page, more := session.HistoryPage(msgs, b, limit)
	// Strip the server-injected scaffolding from user messages so replayed history
	// matches what the live view showed (and never re-surfaces hidden instructions).
	// A user row that is ENTIRELY scaffolding (the autonomous job-notify turn) strips
	// to empty — drop it so history shows no bubble the live view never did. The
	// digest (count/hash) is over the full stored transcript, not this display page,
	// so omitting a display row doesn't disturb the app's freshness check or paging
	// (each kept row carries its own transcript index).
	kept := page[:0]
	for i := range page {
		if page[i].Role == "user" {
			page[i].Text = stripInjected(page[i].Text)
			if strings.TrimSpace(page[i].Text) == "" {
				continue
			}
		}
		kept = append(kept, page[i])
	}
	c.send(msgHistory(recID(s), name, kept, more, count, hash, false))
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
	var hits atomic.Int64
	started := time.Now()
	for i, s := range sessions {
		wg.Add(1)
		go func(i int, s *session.Session) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// DisplayDigest, not ReadDisplayHistory: when the session's transcripts
			// haven't moved this is a few stats instead of a full parse.
			count, hash, cached, err := c.srv.driver.DisplayDigest(s)
			if err != nil {
				return // unreadable: the app keeps whatever it already cached
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
	log.Printf("digest sweep: %d session(s), %d cached, %d recomputed in %v",
		len(sessions), hits.Load(), int64(len(out))-hits.Load(), time.Since(started).Round(time.Millisecond))
	c.send(msgDigests(out))
}

// commandHelp is spoken + shown when the user asks "hey buddy help".
const commandHelp = "here's what I know: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, clear the context, compress the context, " +
	"list models, use model by number, stop the turn, cancel message, and help. " +
	"say hey buddy, then the command, then your end token."

	// sandboxTarget returns the session's target string only when it's a sandbox
	// session (the non-default target the app badges); "" for host sessions.

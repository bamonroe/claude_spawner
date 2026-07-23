package gateway

import (
	"github.com/bam/claude_spawner/server/internal/session"
)

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.
func (c *conn) serveHistory(name string, before *int, limit int, haveHash string) {
	s := c.srv.store.Get(name)
	if s == nil {
		c.fail("no_session", "no such session: "+name)
		return
	}
	msgs, err := c.srv.driver.ReadDisplayHistory(s)
	if err != nil {
		c.fail("history_failed", err.Error())
		return
	}
	count, hash := session.HistoryDigest(msgs)
	// Cache-validation fast path: a top-page request (before == nil) carrying the
	// hash the app already holds needs no message bodies — tell it the cache is
	// current so clicking back into an unchanged session transfers nothing.
	if before == nil && haveHash != "" && haveHash == hash {
		c.send(msgHistory(name, nil, false, count, hash, true))
		return
	}
	b := -1
	if before != nil {
		b = *before
	}
	page, more := session.HistoryPage(msgs, b, limit)
	// Strip the server-injected scaffolding from user messages so replayed history
	// matches what the live view showed (and never re-surfaces hidden instructions).
	for i := range page {
		if page[i].Role == "user" {
			page[i].Text = stripInjected(page[i].Text)
		}
	}
	c.send(msgHistory(name, page, more, count, hash, false))
}

// serveDigests reports every registered session's transcript digest (message
// count + content hash) so the app can validate its offline transcript cache on
// connect without transferring any message bodies — it refetches history only
// for sessions whose hash changed. Transcript reads are memoized by file stat,
// so recomputing digests when nothing changed is cheap. An unreadable session is
// skipped (the app keeps whatever it already cached for it).

// serveDigests reports every registered session's transcript digest (message
// count + content hash) so the app can validate its offline transcript cache on
// connect without transferring any message bodies — it refetches history only
// for sessions whose hash changed. Transcript reads are memoized by file stat,
// so recomputing digests when nothing changed is cheap. An unreadable session is
// skipped (the app keeps whatever it already cached for it).
func (c *conn) serveDigests() {
	sessions := c.srv.store.List()
	items := make([]digestView, 0, len(sessions))
	for _, s := range sessions {
		msgs, err := c.srv.driver.ReadDisplayHistory(s)
		if err != nil {
			continue
		}
		count, hash := session.HistoryDigest(msgs)
		items = append(items, digestView{Name: s.Name, SessionID: s.SessionID, Count: count, Hash: hash})
	}
	c.send(msgDigests(items))
}

// commandHelp is spoken + shown when the user asks "hey buddy help".
const commandHelp = "here's what I know: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, clear the context, compress the context, " +
	"list models, use model by number, stop the turn, cancel message, and help. " +
	"say hey buddy, then the command, then your end token."

	// sandboxTarget returns the session's target string only when it's a sandbox
	// session (the non-default target the app badges); "" for host sessions.

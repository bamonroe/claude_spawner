package gateway

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"sync"

	"github.com/bam/claude_spawner/server/internal/session"
)

// logField trims a prompt/reply to one tidy line for the turn log.
func logField(s string) string {
	const max = 500
	out := make([]rune, 0, max)
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= max {
			return string(out) + "…"
		}
	}
	return string(out)
}

// sessionJob is a per-session hub for a dictation turn, running independently of
// any WebSocket connection so a long Claude job survives the app disconnecting.
// It fans out live events (tool breadcrumbs, the final result) to EVERY currently
// attached connection's sink — so several devices watching the same session all
// see the turn live — and buffers the final result if nobody is attached when the
// turn finishes, for delivery on the next reconnect.
//
// The hub persists across turns (it holds the sink set + any buffered result)
// until the session is deleted. Sinks are maintained by bindJob/unbindJob as
// connections attach/detach, independent of which one dictated the turn.
//
// Lock ordering: emit/finish/bindJob call the sinks (conn.jobSink -> conn.send)
// while holding j.mu, and conn.send takes conn.wmu — so the order is ALWAYS
// j.mu -> conn.wmu, never the reverse. A sink must not call back into the job
// (it would re-enter j.mu and deadlock); it only does a websocket write.
type sessionJob struct {
	mu        sync.Mutex
	running   bool
	final     map[string]any           // last turn's result, buffered until delivered
	delivered bool                     // was `final` delivered to at least one live sink?
	sinks     map[*conn]func(any) bool // every attached connection's sink
	cancel    context.CancelFunc       // aborts the running turn's claude child
	aborted   bool                     // set when the current turn was cancelled on request
}

func (j *sessionJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// broadcast sends msg to every attached sink, returning true if at least one
// reached a live client. Call with j.mu held.
func (j *sessionJob) broadcast(msg any) bool {
	reached := false
	for _, sink := range j.sinks {
		if sink(msg) {
			reached = true
		}
	}
	return reached
}

// emit fans out a live-only event (e.g. a tool breadcrumb) to every attached
// connection; dropped for any that are gone / if nobody's attached.
func (j *sessionJob) emit(msg map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.broadcast(msg)
}

func (j *sessionJob) finish(final map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.cancel = nil
	if j.broadcast(final) {
		j.delivered = true // reached at least one live client; no need to buffer
	} else {
		j.final = final // nobody attached — buffer for the next reconnect
	}
}

// jobFor returns the session's job hub, creating it if absent. The hub persists
// across turns (it holds the attached-connection sinks and any buffered result)
// until the session is deleted (dropJob).
func (s *Server) jobFor(name string) *sessionJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[name]
	if j == nil {
		j = &sessionJob{}
		s.jobs[name] = j
	}
	return j
}

// startTurn launches a dictation turn for the session in the background (so it
// outlives the connection) and fans events out to every attached connection.
// Returns false if a turn is already running for that session.
func (s *Server) startTurn(sess *session.Session, text string) bool {
	j := s.jobFor(sess.Name)
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return false
	}
	// Background-derived (so the turn outlives the connection) but cancelable, so
	// "abort" can kill the claude child on demand.
	ctx, cancel := context.WithCancel(context.Background())
	j.running = true
	j.final = nil // a fresh turn supersedes any prior buffered result
	j.delivered = false
	j.aborted = false
	j.cancel = cancel
	j.mu.Unlock()

	s.inflight.add(sess.Name) // persist "running" so a restart can flag it interrupted
	log.Printf("turn[%s] input: %q", sess.Name, logField(text))
	go func() {
		defer s.inflight.remove(sess.Name)
		j.emit(msgActivity("🤔 thinking…"))
		changed := map[string]bool{}
		onTool := func(t session.ToolUse) {
			if isFileTool(t.Name) && t.FilePath != "" {
				base := filepath.Base(t.FilePath)
				changed[base] = true
				j.emit(msgActivity("✏️ editing " + base))
			} else {
				j.emit(msgActivity(toolActivity(t.Name)))
			}
		}
		wasStarted := sess.Started // Turn flips Started true on the first success
		reply, err := s.driver.Turn(ctx, sess, text, onTool)
		if len(changed) > 0 {
			j.emit(msgFiles(sortedKeys(changed))) // persistent "edited: …" chip
		}
		if err != nil {
			j.mu.Lock()
			aborted := j.aborted
			j.mu.Unlock()
			if aborted {
				log.Printf("turn[%s] stopped on request", sess.Name)
				j.finish(msgTurnStopped(sess.Name))
				return
			}
			log.Printf("turn[%s] error: %v", sess.Name, err)
			j.finish(msgError("turn_failed", err.Error()))
			return
		}
		log.Printf("turn[%s] reply: %q", sess.Name, logField(reply))
		if !wasStarted {
			// Only the first turn changes the record (Started false->true, for
			// --resume). Later turns leave it identical, so skip re-serializing and
			// rewriting the whole store to disk on every turn.
			_ = s.store.Put(sess)
		}
		j.finish(msgOutput(sess.Name, reply, false))
	}()
	return true
}

func isFileTool(name string) bool {
	switch name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

// toolActivity maps a tool name to a friendly live-activity line.
func toolActivity(name string) string {
	switch name {
	case "Bash":
		return "⚙️ running a command…"
	case "Read":
		return "📖 reading a file…"
	case "Grep", "Glob":
		return "🔍 searching the code…"
	case "WebFetch", "WebSearch":
		return "🌐 searching the web…"
	case "Task":
		return "🤖 running a subtask…"
	case "":
		return "🤔 working…"
	default:
		return "· " + name + "…"
	}
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// bindJob adds a newly-attached connection to the session's job (creating the hub
// if needed, so future turns fan out to this connection) and catches it up: a
// running job sends this one connection a "still working" nudge; a finished-but-
// undelivered job hands it the buffered result.
func (s *Server) bindJob(c *conn, sessName string, silent bool) {
	j := s.jobFor(sessName)
	sink := c.jobSink()
	// A turn that was running when the server last restarted is dead; tell the app
	// once so it doesn't wait on it (its result, if any, is in the transcript the
	// app reloads on attach).
	if s.takeInterrupted(sessName) {
		sink(msgTurnInterrupted(sessName, "server restarted"))
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sinks == nil {
		j.sinks = map[*conn]func(any) bool{}
	}
	j.sinks[c] = sink
	switch {
	case j.running:
		// Catch up just this new connection (not a fan-out). Silent reconnect
		// auto-attach gets a quiet breadcrumb (so the app knows the turn survived
		// and its interruption watchdog resets); a voice attach gets a spoken nudge.
		if silent {
			sink(msgActivity("🤔 still working…"))
		} else {
			sink(msgSay("still working on it, bud — one sec."))
		}
	case !j.delivered && j.final != nil:
		// A turn finished with nobody attached; hand the buffered result to the
		// first connection back, then free it.
		if sink(j.final) {
			j.delivered = true
			j.final = nil
		}
	}
}

// unbindJob removes a connection's sink from the session job (on disconnect or
// detach). The hub and any buffered result survive so a turn that finishes while
// nobody is attached is still delivered on the next reconnect.
func (s *Server) unbindJob(c *conn, sessName string) {
	s.jobsMu.Lock()
	j := s.jobs[sessName]
	s.jobsMu.Unlock()
	if j != nil {
		j.mu.Lock()
		delete(j.sinks, c)
		j.mu.Unlock()
	}
}

// renameJob re-keys a session's job hub when the session is renamed, so its
// sinks, in-flight turn, and buffered result carry over to the new name.
func (s *Server) renameJob(old, newName string) {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if j := s.jobs[old]; j != nil {
		delete(s.jobs, old)
		s.jobs[newName] = j
	}
}

// takeInterrupted reports (and clears) whether a session's turn was cut off by
// the last server restart, so the app is told once on re-attach.
func (s *Server) takeInterrupted(name string) bool {
	s.interruptedMu.Lock()
	defer s.interruptedMu.Unlock()
	if s.interrupted[name] {
		delete(s.interrupted, name)
		return true
	}
	return false
}

// cancelTurn aborts a session's running turn (kills the claude child). Returns
// true if a turn was running and got cancelled.
func (s *Server) cancelTurn(name string) bool {
	s.jobsMu.Lock()
	j := s.jobs[name]
	s.jobsMu.Unlock()
	if j == nil {
		return false
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.running || j.cancel == nil {
		return false
	}
	j.aborted = true
	j.cancel()
	return true
}

// dropJob forgets a session's job (e.g. when the session is deleted).
func (s *Server) dropJob(sessName string) {
	s.jobsMu.Lock()
	delete(s.jobs, sessName)
	s.jobsMu.Unlock()
}

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

// sessionJob is a dictation turn that runs independently of any WebSocket
// connection, so a long Claude job survives the app disconnecting. Live events
// (tool breadcrumbs, the final result) go to the attached connection if there is
// one; the final result is buffered and delivered on the next reconnect if not.
//
// The `live` sink returns true only if it actually reached a connected client —
// so if the app drops right as the turn finishes, the result stays undelivered
// and is replayed when the app comes back.
type sessionJob struct {
	mu        sync.Mutex
	running   bool
	final     map[string]any // result or error message, once done
	delivered bool           // was `final` delivered to a live connection?
	live      func(any) bool // current attached connection's sink, or nil
}

func (j *sessionJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// emit sends a live-only event (e.g. a tool breadcrumb); dropped if nobody's attached.
func (j *sessionJob) emit(msg map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.live != nil {
		j.live(msg)
	}
}

func (j *sessionJob) finish(final map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	if j.live != nil && j.live(final) {
		j.delivered = true // reached a live client; no need to buffer it
	} else {
		j.final = final // nobody attached — buffer for the next reconnect
	}
}

// startTurn launches a dictation turn for the session in the background (so it
// outlives the connection) and streams events to `live`. Returns false if a turn
// is already running for that session.
func (s *Server) startTurn(sess *session.Session, text string, live func(any) bool) bool {
	s.jobsMu.Lock()
	if j := s.jobs[sess.Name]; j != nil && j.isRunning() {
		s.jobsMu.Unlock()
		return false
	}
	j := &sessionJob{running: true, live: live}
	s.jobs[sess.Name] = j
	s.jobsMu.Unlock()

	log.Printf("turn[%s] input: %q", sess.Name, logField(text))
	go func() {
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
		reply, err := s.driver.Turn(context.Background(), sess, text, onTool)
		if len(changed) > 0 {
			j.emit(msgFiles(sortedKeys(changed))) // persistent "edited: …" chip
		}
		if err != nil {
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

// bindJob wires a newly-attached connection's sink to the session's job (if any)
// and catches it up: a running job says "still working", a finished-but-
// undelivered job delivers its buffered result.
func (s *Server) bindJob(sessName string, live func(any) bool, silent bool) {
	s.jobsMu.Lock()
	j := s.jobs[sessName]
	s.jobsMu.Unlock()
	if j == nil {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.live = live
	switch {
	case j.running:
		if silent {
			// Reconnect auto-attach: no spoken line, but send a silent breadcrumb so
			// the app knows the turn survived the disconnect (and its interruption
			// watchdog resets) rather than assuming the turn was lost.
			live(msgActivity("🤔 still working…"))
		} else {
			live(msgSay("still working on it, bud — one sec."))
		}
	case !j.delivered && j.final != nil:
		if live(j.final) {
			j.delivered = true
			j.final = nil // free the buffered reply once it's delivered
		}
	}
}

// unbindJob clears a session job's live sink (on disconnect/detach) so a job that
// finishes while nobody is attached buffers its result for the next reconnect.
func (s *Server) unbindJob(sessName string) {
	s.jobsMu.Lock()
	j := s.jobs[sessName]
	s.jobsMu.Unlock()
	if j != nil {
		j.mu.Lock()
		j.live = nil
		j.mu.Unlock()
	}
}

// dropJob forgets a session's job (e.g. when the session is deleted).
func (s *Server) dropJob(sessName string) {
	s.jobsMu.Lock()
	delete(s.jobs, sessName)
	s.jobsMu.Unlock()
}

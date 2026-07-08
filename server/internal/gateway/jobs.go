package gateway

import (
	"context"
	"log"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// diffSummary returns a compact `git diff --stat` of the working tree in dir, or
// "" if dir isn't a git repo, has no uncommitted changes, or on any error. Capped
// so a huge diff can't flood the app.
func diffSummary(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--stat", "--stat-width=60").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 12 {
		lines = append(lines[:11], "… (+"+strconv.Itoa(len(lines)-11)+" more)")
	}
	return strings.Join(lines, "\n")
}

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

// broadcastExcept sends msg to every attached sink except `origin`'s.
func (j *sessionJob) broadcastExcept(origin *conn, msg any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for c, sink := range j.sinks {
		if c != origin {
			sink(msg)
		}
	}
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
func (s *Server) jobFor(sessID string) *sessionJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	j := s.jobs[sessID]
	if j == nil {
		j = &sessionJob{}
		s.jobs[sessID] = j
	}
	return j
}

// startTurn launches a dictation turn for the session in the background (so it
// outlives the connection) and fans events out to every attached connection.
// Returns false if a turn is already running for that session.
// primeAsk is true when this turn's text carries the interactive ask instruction
// (the first interactive turn of a context); on success the session is marked
// AskPrimed so later turns omit it.
func (s *Server) startTurn(sess *session.Session, text string, primeAsk bool) bool {
	j := s.jobFor(sess.SessionID)
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

	s.inflight.add(sess.SessionID) // persist "running" so a restart can flag it interrupted
	log.Printf("turn[%s] input: %q", sess.Name, logField(text))
	go func() {
		defer s.inflight.remove(sess.SessionID)
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
		// Stream Claude's prose live (chunk=true) as each assistant message lands, so
		// the app shows/speaks it as it's produced rather than all at once at the end.
		// The interactive-mode ASK sentinel is NOT streamed — it's delivered as a
		// structured `ask` when the turn finishes (see parseAsk below).
		onText := func(prose string) {
			if strings.Contains(prose, "::ASK::") {
				return
			}
			j.emit(msgOutput(sess.Name, prose, true, nil))
		}
		// The rate_limit_event lands early in the stream; broadcast the plan's
		// session-limit state to every attached device as soon as it arrives, and
		// feed the 5-hour reset time to the usage estimator (so it can detect a
		// window rollover and restart the drift from zero).
		onRateLimit := func(rl session.RateLimit) {
			s.setRateLimit(rl)
			if rl.Type == "five_hour" {
				s.usage.NoteSessionResetsAt(rl.ResetsAt)
			}
			j.emit(msgRateLimit(rl))
		}
		wasStarted := sess.Started // Turn flips Started true on the first success
		reply, turnUsage, err := s.driver.Turn(ctx, sess, text, onTool, onText, onRateLimit)
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
			// A failed turn that nonetheless launched claude created the session on
			// disk (Turn flips Started on launch). Persist that — and drop the seed
			// it consumed — so the next turn resumes instead of re-attempting
			// --session-id on an id claude already owns (which fails forever).
			if sess.Started != wasStarted {
				sess.PendingSeed = ""
				_ = s.store.Put(sess)
			}
			if spoken := spokenError["turn_failed"]; spoken != "" {
				j.emit(msgSay(spoken)) // don't leave a voice user with a silent failure
			}
			j.finish(msgError("turn_failed", err.Error()))
			return
		}
		log.Printf("turn[%s] reply: %q", sess.Name, logField(reply))
		// Feed this turn's token cost into the server-global usage estimate (all
		// sessions/clients) and push the drifted estimate to every connected app.
		s.broadcastUsageEstimate(s.usage.AddTurn(tokenCost(turnUsage)))
		// The first turn flips Started false->true (for --resume); the first
		// interactive turn primes AskPrimed so the instruction isn't re-sent. Either
		// change means we persist; an unchanged record skips the disk rewrite.
		changedRec := !wasStarted
		if primeAsk && !sess.AskPrimed {
			sess.AskPrimed = true
			changedRec = true
		}
		// A compress-carried seed was prepended to this turn (see dictate); the fresh
		// session_id now holds that context via --resume, so clear it — it must not be
		// re-injected on later turns. Cleared only on success, so a failed turn retries
		// with the seed intact.
		if sess.PendingSeed != "" {
			sess.PendingSeed = ""
			changedRec = true
		}
		if changedRec {
			_ = s.store.Put(sess)
		}
		if len(changed) > 0 { // a compact review summary of what the turn touched
			if d := diffSummary(sess.Dir); d != "" {
				j.emit(msgDiff(d))
			}
		}
		if qs, ok := parseAsk(reply); ok {
			// Interactive mode: Claude wants clarification — deliver the questions
			// for the app to render/read, not as a final answer.
			j.finish(msgAsk(sess.Name, qs))
			return
		}
		// The context-size badge must reflect the CURRENT context window, not the turn's
		// AGGREGATE usage. The result event sums every internal tool-step of an agentic
		// turn (each re-reads the whole context), so turnUsage can be many times the real
		// context and bounces with tool-use count. Read the true size the way attach does
		// — the transcript's last assistant message — so live matches on-attach.
		// (turnUsage still feeds the cumulative spend estimate above, where summing is
		// what we want.)
		badge := turnUsage
		if cx := s.driver.LastContextUsage(sess.Host, sess.TranscriptIDs()); cx != nil {
			badge = cx.Usage
		}
		j.finish(msgOutput(sess.Name, reply, false, &badge))
	}()
	return true
}

// compressPrompt asks Claude to distill the current context into a compact
// briefing that can seed a fresh session. It is sent as a normal turn on the
// session_id being retired; the reply becomes the PendingSeed carried forward.
const compressPrompt = "Summarize our conversation so far into a compact but complete briefing that a " +
	"fresh session with no prior memory could use to continue seamlessly. Capture the task/goal, key " +
	"decisions and their rationale, the current state, important file paths and code specifics, and any " +
	"open threads or next steps. Be thorough on specifics but drop small talk. Output only the summary " +
	"prose — no preamble, no tool use."

// startCompress compacts the attached session's Claude context in the background:
// it asks Claude (on the current session_id) to summarize the conversation, then
// rotates to a fresh session_id and stashes that summary as PendingSeed, so the
// next dictation carries the condensed context forward. Unlike startTurn's
// dictation, the summary itself is never spoken/shown as a reply — only an
// activity breadcrumb while it runs and a final `say` confirmation. Returns false
// if a turn is already running for the session (the single-writer invariant).
func (s *Server) startCompress(sess *session.Session) bool {
	j := s.jobFor(sess.SessionID)
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return false
	}
	// Background-derived so the summary outlives the connection, but cancelable so
	// "abort" can kill it like any turn.
	ctx, cancel := context.WithCancel(context.Background())
	j.running = true
	j.final = nil
	j.delivered = false
	j.aborted = false
	j.cancel = cancel
	j.mu.Unlock()

	s.inflight.add(sess.SessionID)
	log.Printf("compress[%s] summarizing", sess.Name)
	go func() {
		defer s.inflight.remove(sess.SessionID)
		j.emit(msgActivity("🗜️ compressing context…"))
		onRateLimit := func(rl session.RateLimit) {
			s.setRateLimit(rl)
			if rl.Type == "five_hour" {
				s.usage.NoteSessionResetsAt(rl.ResetsAt)
			}
			j.emit(msgRateLimit(rl))
		}
		summary, cUsage, err := s.driver.Turn(ctx, sess, compressPrompt, nil, nil, onRateLimit)
		if err != nil {
			j.mu.Lock()
			aborted := j.aborted
			j.mu.Unlock()
			if aborted {
				log.Printf("compress[%s] stopped on request", sess.Name)
				j.finish(msgTurnStopped(sess.Name))
				return
			}
			log.Printf("compress[%s] error: %v", sess.Name, err)
			if spoken := spokenError["compress_failed"]; spoken != "" {
				j.emit(msgSay(spoken))
			}
			j.finish(msgError("compress_failed", err.Error()))
			return
		}
		// The summary turn consumed usage too — count it toward the estimate.
		s.broadcastUsageEstimate(s.usage.AddTurn(tokenCost(cUsage)))
		// Rotate: retire the just-summarized session_id (kept on disk for history)
		// and start fresh, carrying the summary forward as PendingSeed for the next
		// dictation. driver.Turn flipped Started true for the summary turn; the
		// rotation resets it so the seed turn recreates the session with --session-id.
		newID, err := session.NewSessionID()
		if err != nil {
			j.finish(msgError("internal", err.Error()))
			return
		}
		oldID := sess.SessionID
		sess.PriorIDs = append(sess.PriorIDs, sess.SessionID)
		sess.SessionID = newID
		sess.Started = false
		sess.AskPrimed = false // fresh context: re-prime the ask instruction on the next turn
		sess.PendingSeed = strings.TrimSpace(summary)
		if err := s.store.Put(sess); err != nil {
			j.finish(msgError("internal", err.Error()))
			return
		}
		// The session_id rotated: move the hub (holds this connection's sink) and the
		// id index onto the new id so the seeded next turn reaches the same devices.
		s.rekeyJob(oldID, newID)
		_ = s.store.ForgetID(oldID)
		log.Printf("compress[%s] rotated to %s (seed %d bytes)", sess.Name, newID, len(sess.PendingSeed))
		j.emit(msgContextReset(sess.Name)) // reset the app's context-size readout; the seeded turn sets the new size
		j.finish(msgSay("compressed. carried a summary forward — your history is still here."))
	}()
	return true
}

// Per-token weights approximating how Anthropic meters plan usage (cost-relative
// to a plain input token). A cache READ is billed at ~0.1× input, so counting it
// at full weight — as this used to — made the (huge) cached-context re-read every
// turn dominate the measure and drift the estimate up ~10× too fast: on a big
// (~1M-token) context a single turn's ~1M cache-read tokens would eat a quarter of
// the seeded session budget, so a couple of turns pegged the estimate at 100%.
// A cache WRITE is ~1.25× input. The estimator calibrates against real /usage, so
// these need only be roughly right, but cache_read's ~10× overcount was not.
const (
	weightCacheWrite = 1.25
	weightCacheRead  = 0.10
)

// tokenCost is the weighted per-turn token measure fed to the usage estimator:
// every token the turn touched, with cache reads/writes weighted toward their real
// metered cost (see the weights above) rather than counted flat. The dominant term
// on a warm session is the cached context re-read, which we discount so it tracks
// plan consumption instead of raw context size.
func tokenCost(u session.Usage) int64 {
	return int64(float64(u.Input+u.Output) +
		weightCacheWrite*float64(u.CacheWrite) +
		weightCacheRead*float64(u.CacheRead))
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
func (s *Server) bindJob(c *conn, sess *session.Session, silent bool) {
	j := s.jobFor(sess.SessionID)
	sink := c.jobSink()
	// A turn that was running when the server last restarted is dead; tell the app
	// once so it doesn't wait on it (its result, if any, is in the transcript the
	// app reloads on attach).
	if s.takeInterrupted(sess.SessionID) {
		sink(msgTurnInterrupted(sess.Name, "server restarted"))
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
			sink(msgSay("still working on it — one sec."))
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
func (s *Server) unbindJob(c *conn, sessID string) {
	s.jobsMu.Lock()
	j := s.jobs[sessID]
	s.jobsMu.Unlock()
	if j != nil {
		j.mu.Lock()
		delete(j.sinks, c)
		j.mu.Unlock()
	}
}

// isBusy reports whether a dictation turn is running for the session now.
func (s *Server) isBusy(sessID string) bool {
	s.jobsMu.Lock()
	j := s.jobs[sessID]
	s.jobsMu.Unlock()
	return j != nil && j.isRunning()
}

// echoUserPrompt shows a just-dictated prompt on the OTHER devices attached to a
// session (the dictating one already displayed it), so multi-device views stay
// in sync live rather than only on the next history reload.
func (s *Server) echoUserPrompt(sessID, text string, origin *conn) {
	s.jobsMu.Lock()
	j := s.jobs[sessID]
	s.jobsMu.Unlock()
	if j != nil {
		j.broadcastExcept(origin, msgTranscript(text, true))
	}
}

// takeInterrupted reports (and clears) whether a session's turn was cut off by
// the last server restart, so the app is told once on re-attach.
func (s *Server) takeInterrupted(sessID string) bool {
	s.interruptedMu.Lock()
	defer s.interruptedMu.Unlock()
	if s.interrupted[sessID] {
		delete(s.interrupted, sessID)
		return true
	}
	return false
}

// cancelTurn aborts a session's running turn (kills the claude child). Returns
// true if a turn was running and got cancelled.
func (s *Server) cancelTurn(sessID string) bool {
	s.jobsMu.Lock()
	j := s.jobs[sessID]
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
func (s *Server) dropJob(sessID string) {
	s.jobsMu.Lock()
	delete(s.jobs, sessID)
	s.jobsMu.Unlock()
}

// rekeyJob moves a session's job hub from oldID to newID, preserving its sinks,
// any in-flight turn, and buffered result. The hub is keyed by session_id, but a
// compact/clear ROTATES the session_id while the logical session — and the
// connections attached to it — carry across; without this re-key the next turn
// would fan out on a fresh hub the attached connections aren't bound to.
func (s *Server) rekeyJob(oldID, newID string) {
	if oldID == newID {
		return
	}
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	if j := s.jobs[oldID]; j != nil {
		delete(s.jobs, oldID)
		s.jobs[newID] = j
	}
}

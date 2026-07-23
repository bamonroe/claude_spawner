package gateway

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bam/claude_spawner/server/internal/session"
)

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
// AskPrimed so later turns omit it. primeJobs is the same for the background-job
// instruction (marks JobsPrimed).
func (s *Server) startTurn(sess *session.Session, text string, primeAsk, primeJobs bool) bool {
	j := s.jobFor(sess.SessionID)
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return false
	}
	// Background-derived (so the turn outlives the connection) but cancelable, so
	// "abort" can kill the claude child on demand.
	ctx, cancel := context.WithCancel(context.Background())
	j.beginTurn(cancel)
	j.mu.Unlock()

	s.inflight.add(sess.SessionID) // persist "running" so a restart can flag it interrupted
	turnID := newTurnID()          // shared by every output frame of this turn — the client's dedup key
	log.Printf("turn[%s] input: %q", sess.Name, logField(text))
	go func() {
		defer s.inflight.remove(sess.SessionID)
		j.flushPending() // redeliver an earlier reply whose send failed, now that we're writing again
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
			j.emit(msgOutput(sess.Name, prose, turnID, true, nil))
		}
		// The rate_limit_event lands early in the stream; broadcast the plan's
		// session-limit state to every attached device as soon as it arrives.
		onRateLimit := func(rl session.RateLimit) {
			s.setRateLimit(rl)
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
				j.finish(stampTurn(msgTurnStopped(sess.Name), turnID))
				return
			}
			log.Printf("turn[%s] error: %v", sess.Name, err)
			// A failed turn that nonetheless launched claude created the session on
			// disk (Turn flips Started on launch). Persist that — and drop the seed
			// it consumed — so the next turn resumes instead of re-attempting
			// --session-id on an id claude already owns (which fails forever).
			if sess.Started != wasStarted {
				sess.PendingSeed = ""
				if perr := s.store.Put(sess); perr != nil {
					log.Printf("turn[%s] persist after failed turn: %v", sess.Name, perr)
				}
			}
			if spoken := spokenError["turn_failed"]; spoken != "" {
				j.emit(msgSay(spoken)) // don't leave a voice user with a silent failure
			}
			j.finish(stampTurn(msgError("turn_failed", err.Error()), turnID))
			return
		}
		log.Printf("turn[%s] reply: %q", sess.Name, logField(reply))
		// The first turn flips Started false->true (for --resume); the first
		// interactive turn primes AskPrimed so the instruction isn't re-sent. Either
		// change means we persist; an unchanged record skips the disk rewrite.
		changedRec := !wasStarted
		if primeAsk && !sess.AskPrimed {
			sess.AskPrimed = true
			changedRec = true
		}
		if primeJobs && !sess.JobsPrimed {
			sess.JobsPrimed = true
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
			if perr := s.store.Put(sess); perr != nil {
				log.Printf("turn[%s] persist: %v", sess.Name, perr)
			}
		}
		if len(changed) > 0 { // a compact review summary of what the turn touched
			if d := diffSummary(sess.Dir); d != "" {
				j.emit(msgDiff(d))
			}
		}
		if qs, ok := parseAsk(reply); ok {
			// Interactive mode: Claude wants clarification — deliver the questions
			// for the app to render/read, not as a final answer.
			j.finish(stampTurn(msgAsk(sess.Name, qs), turnID))
			return
		}
		// The context-size badge must reflect the CURRENT context window, not the turn's
		// AGGREGATE usage. The result event sums every internal tool-step of an agentic
		// turn (each re-reads the whole context), so turnUsage can be many times the real
		// context and bounces with tool-use count. Read the true size the way attach does
		// — the transcript's last assistant message — so live matches on-attach.
		badge := turnUsage
		if cx := s.driver.LastContextUsage(sess.Agent, sess.Host, sess.TranscriptIDs()); cx != nil {
			badge = cx.Usage
		}
		j.finish(msgOutput(sess.Name, reply, turnID, false, &badge))
	}()
	return true
}

// compressPrompt asks Claude to distill the current context into a compact
// briefing that can seed a fresh session. It is sent as a normal turn on the
// session_id being retired; the reply becomes the PendingSeed carried forward.
const compressPrompt = "Summarize our conversation so far into a compact but complete briefing that a " +
	"fresh session with no prior memory could use to continue seamlessly. Capture the task/goal, key " +
	"decisions and their rationale, the current state, important file paths and code specifics, and any " +
	"open threads or next steps. Weight the most recent exchanges disproportionately: preserve the latest " +
	"messages in near-verbatim detail (they are the active working context), and compress older history " +
	"more aggressively the further back it goes. Be thorough on specifics but drop small talk. Output only " +
	"the summary prose — no preamble, no tool use."

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
	j.beginTurn(cancel)
	j.mu.Unlock()

	s.inflight.add(sess.SessionID)
	turnID := newTurnID() // the compress is a turn too — its terminal frames carry an id
	log.Printf("compress[%s] summarizing", sess.Name)
	go func() {
		defer s.inflight.remove(sess.SessionID)
		j.flushPending() // an idle compress must not swallow a reply whose send failed
		j.emit(msgActivity("🗜️ compressing context…"))
		onRateLimit := func(rl session.RateLimit) {
			s.setRateLimit(rl)
			j.emit(msgRateLimit(rl))
		}
		summary, _, err := s.driver.Turn(ctx, sess, compressPrompt, nil, nil, onRateLimit)
		if err != nil {
			j.mu.Lock()
			aborted := j.aborted
			j.mu.Unlock()
			if aborted {
				log.Printf("compress[%s] stopped on request", sess.Name)
				j.finish(stampTurn(msgTurnStopped(sess.Name), turnID))
				return
			}
			log.Printf("compress[%s] error: %v", sess.Name, err)
			if spoken := spokenError["compress_failed"]; spoken != "" {
				j.emit(msgSay(spoken))
			}
			j.finish(stampTurn(msgError("compress_failed", err.Error()), turnID))
			return
		}
		// Rotate: retire the just-summarized session_id (kept on disk for history)
		// and start fresh, carrying the summary forward as PendingSeed for the next
		// dictation. driver.Turn flipped Started true for the summary turn; the
		// rotation resets it so the seed turn recreates the session with --session-id.
		newID, err := session.NewSessionID()
		if err != nil {
			j.finish(stampTurn(msgError("internal", err.Error()), turnID))
			return
		}
		oldID := sess.SessionID
		sess.PriorIDs = append(sess.PriorIDs, sess.SessionID)
		sess.SessionID = newID
		sess.Started = false
		sess.AskPrimed = false  // fresh context: re-prime the ask instruction on the next turn
		sess.JobsPrimed = false // ditto for the background-job instruction (Jobs/PendingNotes survive a compress)
		sess.PendingSeed = strings.TrimSpace(summary)
		if err := s.store.Put(sess); err != nil {
			j.finish(stampTurn(msgError("internal", err.Error()), turnID))
			return
		}
		// The session_id rotated: move the hub (holds this connection's sink) and the
		// id index onto the new id so the seeded next turn reaches the same devices.
		s.rekeyJob(oldID, newID)
		if ferr := s.store.ForgetID(oldID); ferr != nil {
			log.Printf("forget rotated id %s: %v", oldID, ferr)
		}
		log.Printf("compress[%s] rotated to %s (seed %d bytes)", sess.Name, newID, len(sess.PendingSeed))
		// One self-describing reset carrying the rotated session_id (see doClear);
		// the seeded next turn sets the new context size.
		j.emit(msgContextReset(sess.Name, sess.SessionID))
		j.finish(stampTurn(msgSay("compressed. carried a summary forward — your history is still here."), turnID))
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
func (s *Server) bindJob(c *conn, sess *session.Session, silent bool) {
	j := s.jobFor(sess.SessionID)
	// On attach, reconcile detached background jobs so a device that reconnects
	// after a job finished gets the completion breadcrumb and the note is staged for
	// the next dictation. Skip while a turn is running — the reconciler must not race
	// the running turn's store.Put (one-writer); dictate reconciles at the next turn.
	if !j.isRunning() {
		s.reconcileJobs(sess, true)
	}
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
	// Hand back an orphan reply from an earlier turn that reached NO live client
	// (its sends all failed, or nobody was attached). This is done INDEPENDENTLY of
	// whether a new turn is now running — a running turn does not mean the earlier
	// reply was delivered, and skipping it here would strand it. If this new
	// connection can't take it either, fall it into that connection's pending buffer.
	if j.orphan != nil {
		if sink(j.orphan) {
			j.orphan = nil
		} else {
			if j.pending == nil {
				j.pending = map[*conn]map[string]any{}
			}
			j.pending[c] = j.orphan
			j.orphan = nil
		}
	}
	if j.running {
		// Catch up just this new connection (not a fan-out). First replay the streamed
		// prose this turn has produced so far — a device that attached or reconnected
		// mid-turn has none of it, and the in-flight steps aren't in the on-disk
		// transcript yet, so a history refetch can't backfill them. The client dedups
		// replayed frames it already holds (by turn id / text), and once the turn
		// finishes the whole reply lands in history and collapses any live overlap.
		j.replayInFlight(sink)
		// Then the "still working" breadcrumb. Silent reconnect auto-attach gets a quiet
		// one (so the app knows the turn survived and its interruption watchdog resets);
		// a voice attach gets a spoken nudge.
		if silent {
			sink(msgActivity("🤔 still working…"))
		} else {
			sink(msgSay("still working on it — one sec."))
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
		delete(j.pending, c) // it reloads history on reconnect; no per-conn buffer to keep
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

package gateway

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The background-job completion notifier closes the gap that made "I'll tell you
// when it finishes" a promise the server couldn't keep. Detached jobs (spawner-job)
// are fire-and-forget on the target — nothing watches them; completion was only ever
// discovered lazily, at the START of the user's NEXT dictation (or on a device
// re-attach). So a user who did what the priming line invites — start a job and WAIT
// to be told — was never told, because the only trigger was speaking again.
//
// jobReconcileLoop is the server-owned fix: an idle ticker that reconciles every
// started session's on-target job registry independent of turns, and when a job has
// finished drives an autonomous notify turn so the user actually gets told out loud.
// It also makes the SSH path self-healing: a poll that fails on a dropped pooled
// connection just retries on the next tick instead of vanishing silently.

// jobReconcileTick is how often the idle notifier re-scans started sessions for
// finished background jobs. Slower than autoCompressTick — job completion isn't as
// latency-sensitive, and each scan is an SSH `list` per session, so we keep it light.
const jobReconcileTick = 12 * time.Second

// jobReconcileLoop is the server-owned monitor that turns a finished detached job
// into a spoken heads-up without waiting for the user to speak again. Every tick it
// scans started, idle sessions; reconcileJobs stages any completion note (and reaps
// the job); and when a session has pending notes AND a device is attached, it drives
// an autonomous notify turn so the user hears the result. With nobody attached the
// note simply waits in PendingNotes for the next dictation/attach (the fallback), so
// we never narrate to an empty room. Started once from New().
func (s *Server) jobReconcileLoop() {
	t := time.NewTicker(jobReconcileTick)
	defer t.Stop()
	for range t.C {
		for _, sess := range s.store.List() {
			// Only started sessions can have launched a job and can be --resumed for a
			// notify turn; skip any with a turn already in flight (the one-writer
			// invariant — reconcile must not race a running turn's store.Put).
			if !sess.Started || s.isBusy(sess.SessionID) {
				continue
			}
			// Poll without re-staging the wrapper (stage=false): the last turn already
			// staged it, and re-writing it over SSH every 12s would be pure waste.
			s.reconcileJobs(sess, false)
			if len(sess.PendingNotes) == 0 {
				continue // nothing finished since we last looked
			}
			// Someone has to be listening for an out-loud notice to mean anything. With
			// no attached device, leave the note in PendingNotes — the next dictation or
			// attach surfaces it.
			j := s.jobFor(sess.SessionID)
			if !j.hasSink() {
				continue
			}
			// Announce ALL pending notes (this scan's plus any that accumulated while no
			// device was attached). startJobNotify clears them on success, re-checks the
			// running flag under the lock, and is a no-op if a dictate raced in first.
			s.startJobNotify(sess, append([]string(nil), sess.PendingNotes...))
		}
	}
}

// jobNotifyPrompt frames finished-job notes as an autonomous update turn: unlike
// dictate's preamble (which precedes real user text), this IS the whole turn — there
// is no user message — so it tells Claude to speak the outcome directly and briefly,
// and not to go off and do more work on its own.
func jobNotifyPrompt(notes []string) string {
	return "[Autonomous update — the user did NOT send this message; the server is " +
		"notifying you that a background job you started earlier has now finished.]\n\n" +
		strings.Join(notes, "\n") +
		"\n\n[Give the user a brief spoken heads-up that it finished and, from the output " +
		"above, whether it succeeded or failed — a sentence or two. Do not take any further " +
		"action or use tools; just report the result. The user's next message follows " +
		"separately.]"
}

// startJobNotify drives an autonomous turn that tells the user a detached background
// job has finished. Modelled on startCompress: background-derived so it outlives the
// connection, cancelable so "abort" kills it, and single-writer (returns false if a
// turn is already running). The reply is delivered through the hub, so it reaches
// every attached device or falls into the orphan buffer for the next attach.
//
// PendingNotes are cleared only on SUCCESS — the finished-job context now lives in
// this session_id's transcript, so the next dictation must not re-inject it. On any
// failure the notes are LEFT INTACT so the next dictation still carries the update:
// the job was already reaped on the target, so PendingNotes is the only durable
// record of the completion and we must not drop it on a transient turn failure.
func (s *Server) startJobNotify(sess *session.Session, notes []string) bool {
	if len(notes) == 0 {
		return false
	}
	j := s.jobFor(sess.SessionID)
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return false // a dictate/compress raced in; it will carry the notes itself
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.beginTurn(cancel)
	j.mu.Unlock()

	s.inflight.add(sess.SessionID)
	turnID := newTurnID()
	log.Printf("jobnotify[%s] announcing %d finished job(s)", sess.Name, len(notes))
	go func() {
		defer s.inflight.remove(sess.SessionID)
		j.flushPending() // redeliver an earlier reply whose send failed, now that we're writing again
		j.emit(msgActivity("📣 a background job finished…"))
		onRateLimit := func(rl session.RateLimit) {
			s.setRateLimit(rl)
			j.emit(msgRateLimit(rl))
		}
		// Stream the prose live like a dictation reply so it's spoken as it lands.
		onText := func(prose string) {
			if strings.Contains(prose, "::ASK::") {
				return
			}
			j.emit(msgOutput(sess.Name, prose, turnID, true, nil))
		}
		reply, turnUsage, err := s.driver.Turn(ctx, sess, jobNotifyPrompt(notes), nil, onText, onRateLimit)
		if err != nil {
			j.mu.Lock()
			aborted := j.aborted
			j.mu.Unlock()
			if aborted {
				log.Printf("jobnotify[%s] stopped on request", sess.Name)
				j.finish(stampTurn(msgTurnStopped(sess.Name), turnID))
				return
			}
			// Leave PendingNotes intact — the next dictation still carries the update.
			log.Printf("jobnotify[%s] error: %v", sess.Name, err)
			if spoken := spokenError["turn_failed"]; spoken != "" {
				j.emit(msgSay(spoken))
			}
			j.finish(stampTurn(msgError("turn_failed", err.Error()), turnID))
			return
		}
		log.Printf("jobnotify[%s] reply: %q", sess.Name, logField(reply))
		// Success: the completion is now in this session_id's context (Claude just
		// spoke it), so drop the pending fallback so the next dictation won't re-announce
		// it. Only the notes we announced are cleared — any that arrived while this turn
		// ran are left for the next pass.
		sess.PendingNotes = dropNotes(sess.PendingNotes, notes)
		if perr := s.store.Put(sess); perr != nil {
			log.Printf("jobnotify[%s] persist cleared notes: %v", sess.Name, perr)
		}
		// Read the true context size the way attach/dictate do (last assistant message),
		// not the turn's aggregate usage, so the badge matches.
		badge := turnUsage
		if cx := s.driver.LastContextUsage(sess.Agent, sess.Host, sess.TranscriptIDs()); cx != nil {
			badge = cx.Usage
		}
		j.finish(msgOutput(sess.Name, reply, turnID, false, &badge))
	}()
	return true
}

// dropNotes returns notes with every entry in remove deleted (by exact value). Used
// to clear only the completion notes a notify turn actually announced, leaving any
// that arrived while it ran intact for the next pass.
func dropNotes(notes, remove []string) []string {
	if len(remove) == 0 {
		return notes
	}
	drop := make(map[string]int, len(remove))
	for _, r := range remove {
		drop[r]++
	}
	out := notes[:0]
	for _, n := range notes {
		if drop[n] > 0 {
			drop[n]--
			continue
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

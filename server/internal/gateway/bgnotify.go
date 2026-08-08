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
// the job); and when a session has pending notes it drives an autonomous notify turn
// so the user hears the result. Started once from New().
//
// Whether a detached session (no attached device) fires depends on SPAWNER_EAGER_NOTIFY:
//   - default (off): a notify turn runs only when a device is attached; with nobody
//     listening the note waits in PendingNotes for the next dictation/attach (the
//     fallback), so we never narrate to an empty room.
//   - eager (on): the turn runs the moment the job is detected regardless of
//     attachment, so the agent isn't idle for the gap until the user revisits and the
//     turn is far likelier to land in the token cache window. The spoken reply buffers
//     (the hub's orphan slot) for the next attach.
func (s *Server) jobReconcileLoop() {
	t := time.NewTicker(jobReconcileTick)
	defer t.Stop()
	for range t.C {
		for _, sess := range s.store.List() {
			// Only started sessions can have launched a job and can be --resumed for a
			// notify turn; skip any with a turn already in flight (the one-writer
			// invariant — reconcile must not race a running turn's store.Put).
			// Off-loop ticker: read fields from a snapshot, never the live record
			// (turn goroutines and device read loops mutate it concurrently).
			snap := sess.Snapshot()
			if !snap.Started || s.isBusy(snap.SessionID) {
				continue
			}
			// Poll without re-staging the wrapper (stage=false): the last turn already
			// staged it, and re-writing it over SSH every 12s would be pure waste.
			s.reconcileJobs(sess, false)
			var pending []string // re-read after reconcile: it may have staged new notes
			sess.Read(func(r *session.Session) { pending = append([]string(nil), r.PendingNotes...) })
			if len(pending) == 0 {
				continue // nothing finished since we last looked
			}
			// In the default mode someone has to be listening for an out-loud notice to
			// mean anything: with no attached device, leave the note in PendingNotes so
			// the next dictation/attach surfaces it. In eager mode we fire regardless —
			// the reply buffers to the hub's orphan slot for the next attach.
			j := s.jobFor(snap.SessionID)
			if !j.hasSink() && !s.cfg.EagerNotify {
				continue
			}
			// Announce ALL pending notes (this scan's plus any that accumulated while no
			// device was attached). startJobNotify clears them on success, re-checks the
			// running flag under the lock, and is a no-op if a dictate raced in first.
			s.startJobNotify(sess, pending)
		}
	}
}

// jobNotifyPrompt frames finished-job notes as an autonomous update turn: unlike
// dictate's preamble (which precedes real user text), this IS the whole turn — there
// is no user message — so it tells Claude to speak the outcome directly and briefly,
// and not to go off and do more work on its own.
// jobNotifyMark is the leading marker of the autonomous job-completion prompt below.
// Because the whole prompt is server scaffolding (no user words), stripInjected uses
// this prefix to drop the entire synthetic turn from stored history — otherwise the
// envelope, recorded by Claude as a `user` line, re-surfaces as a bubble on a history
// refetch that was never shown live. It's a package constant so the literal lives in
// exactly one place across the builder and the stripper.
const jobNotifyMark = "[Autonomous update — the user did NOT send this message; the server is " +
	"notifying you that a background job you started earlier has now finished.]"

func jobNotifyPrompt(notes []string) string {
	return jobNotifyMark + "\n\n" +
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
	// One snapshot of the record's identity for this turn: Name/SessionID are read
	// from many points below while other goroutines may rename or rotate the record.
	var name, sessionID string
	sess.Read(func(r *session.Session) { name, sessionID = r.Name, r.SessionID })
	j := s.jobFor(sessionID)
	ctx, cancel := context.WithCancel(context.Background())
	if !j.claimTurn(cancel) {
		cancel()
		return false // a dictate/compress raced in; it will carry the notes itself
	}

	s.inflight.add(sessionID)
	turnID := newTurnID()
	log.Printf("jobnotify[%s] announcing %d finished job(s)", name, len(notes))
	go func() {
		defer s.inflight.remove(sessionID)
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
			j.emit(msgOutput(name, prose, turnID, true, nil, nil))
		}
		res, err := s.driver.Turn(ctx, sess, jobNotifyPrompt(notes), nil, onText, onRateLimit)
		reply, turnUsage := res.Reply, res.Usage
		if err != nil {
			j.mu.Lock()
			aborted := j.aborted
			j.mu.Unlock()
			if aborted {
				log.Printf("jobnotify[%s] stopped on request", name)
				j.finish(stampTurn(msgTurnStopped(name), turnID))
				return
			}
			// Leave PendingNotes intact — the next dictation still carries the update.
			log.Printf("jobnotify[%s] error: %v", name, err)
			if spoken := spokenError["turn_failed"]; spoken != "" {
				j.emit(msgSay(spoken))
			}
			j.finish(stampTurn(msgError("turn_failed", err.Error()), turnID))
			return
		}
		log.Printf("jobnotify[%s] reply: %q", name, logField(reply))
		// Success: the completion is now in this session_id's context (Claude just
		// spoke it), so drop the pending fallback so the next dictation won't re-announce
		// it. Only the notes we announced are cleared — any that arrived while this turn
		// ran are left for the next pass.
		sess.Mutate(func(sess *session.Session) { sess.PendingNotes = dropNotes(sess.PendingNotes, notes) })
		if perr := s.store.Put(sess); perr != nil {
			log.Printf("jobnotify[%s] persist cleared notes: %v", name, perr)
		}
		// Ship the reply first with the turn's own usage as a provisional badge; the
		// true context size (last assistant message in the transcript) is read off the
		// critical path and pushed as a follow-up `context_usage` frame, exactly as a
		// dictated turn does.
		j.finish(msgOutput(name, reply, turnID, false, &turnUsage, &turnStats{Turns: res.Turns, Total: turnUsage}))
		s.pushContextUsage(j, sess)
		// The spoken outcome only reaches devices attached to THIS session. In eager
		// mode the whole point is that nobody is attached, so also push a one-frame
		// heads-up to every device that's connected but looking elsewhere — it
		// surfaces the finished job without them opening the session. Devices already
		// attached are excluded (they just got the real output above).
		s.broadcastNotice(sess, reply)
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

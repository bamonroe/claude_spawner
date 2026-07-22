package gateway

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/session/bgjob"
)

// jobNoteMaxRunes bounds one injected completion note (its log tail included) so a
// runaway job can't blow the turn's token budget — mirrors logField's ~500-rune cap
// on the tail portion. jobTailMaxLines caps the tail line count like diffSummary.
const (
	jobNoteMaxRunes = 500
	jobTailMaxLines = 12
)

// reconcileJobs polls the session's on-target background-job registry (via the same
// transport its turns use), and for every job that has newly finished since the last
// look it appends a bounded completion note to sess.PendingNotes (injected into the
// next turn so Claude learns the job it started earlier is done), marks it Notified,
// reaps it on the target, and persists. A live device gets a breadcrumb.
//
// Concurrency: this MUST be called only when no turn is in flight for the session,
// never alongside an in-flight turn's own store.Put — the one-writer invariant. Safe
// call sites are the top of dictate (before the prompt is built), bindJob (attach),
// and the idle jobReconcileLoop ticker (which checks !isBusy first). Every error is
// swallowed: reconcile can NEVER block or fail a turn. Returns true if it staged a
// completion note for a job that finished since the last look — the ticker uses this
// to decide whether to drive an autonomous "your job finished" notify turn.
//
// stage controls whether the wrapper script is (re)written onto the target first.
// The turn-boundary callers pass true (stage lazily so it's present before Claude
// needs it); the idle ticker passes false — it polls every few seconds and must not
// re-write the script over SSH on every tick (the last turn already staged it; if it
// truly isn't there, list just comes back empty and the next real turn re-stages).
func (s *Server) reconcileJobs(sess *session.Session, stage bool) bool {
	if sess == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	home := session.HostHome()
	script := session.JobScriptPath(home)
	if stage {
		// Stage lazily so the script is present before Claude (or this list) needs it. A
		// staging failure is logged and ignored — it must not block the turn.
		if err := s.srvStageJobScript(ctx, sess, home); err != nil {
			log.Printf("bgjobs[%s]: stage: %v", sess.Name, err)
			// keep going: list will just come back empty if the script truly isn't there
		}
	}

	out, err := s.driver.RunOnTarget(ctx, sess, shellQuoteArg(script)+" list --json")
	if err != nil {
		return false // swallow — target unreachable / no jobs; the ticker retries next tick
	}
	recs, err := bgjob.ParseList(trimToJSONArray(out))
	if err != nil {
		return false // swallow — malformed output
	}

	// Index the server's current view BY POSITION so we can tell newly-finished
	// jobs from ones already announced, and adopt jobs Claude started that we
	// haven't recorded yet. Indexes (not pointers) because the loop appends to
	// sess.Jobs — an append can reallocate the backing array, and a pointer taken
	// before it would keep writing to the dead copy, silently losing the Notified
	// flag and re-announcing the job forever. Drops are deferred past the loop for
	// the same reason: rebuilding the slice mid-loop would invalidate the indexes.
	idx := map[string]int{}
	for i := range sess.Jobs {
		idx[sess.Jobs[i].ID] = i
	}

	changed := false
	var breadcrumbs []string
	var reaped []string
	for _, r := range recs {
		i, ok := idx[r.ID]
		if !ok {
			// A job we hadn't recorded (Claude launched it this or a prior turn) — adopt it.
			sess.Jobs = append(sess.Jobs, session.BackgroundJob{
				ID: r.ID, Cmd: r.Cmd, Started: r.Started, Done: r.Done, ExitCode: r.Exit,
			})
			i = len(sess.Jobs) - 1
			idx[r.ID] = i
			changed = true
		}
		if !r.Done || sess.Jobs[i].Notified {
			continue // still running, or already announced
		}
		// Newly finished: grab a bounded log tail and frame a note.
		tail := ""
		if t, terr := s.driver.RunOnTarget(ctx, sess, shellQuoteArg(script)+" tail "+shellQuoteArg(r.ID)); terr == nil {
			tail = capTail(string(t))
		}
		sess.PendingNotes = append(sess.PendingNotes, jobNote(r.Cmd, tail))
		sess.Jobs[i].Done = true
		sess.Jobs[i].Notified = true
		sess.Jobs[i].ExitCode = r.Exit
		changed = true
		breadcrumbs = append(breadcrumbs, r.Cmd)
		// Reap so logs don't accumulate on the target.
		if _, rerr := s.driver.RunOnTarget(ctx, sess, shellQuoteArg(script)+" reap "+shellQuoteArg(r.ID)); rerr == nil {
			reaped = append(reaped, r.ID) // drop below, after the loop
		}
	}
	// Drop reaped jobs from our view — their work is done and announced.
	for _, id := range reaped {
		sess.Jobs = dropJobByID(sess.Jobs, id)
	}

	if changed {
		if err := s.store.Put(sess); err != nil {
			log.Printf("bgjobs[%s]: persist: %v", sess.Name, err)
		}
	}
	if len(breadcrumbs) > 0 {
		j := s.jobFor(sess.SessionID)
		for _, cmd := range breadcrumbs {
			j.emit(msgActivity("✅ background job finished: " + logField(cmd)))
		}
	}
	return len(breadcrumbs) > 0
}

// srvStageJobScript stages the wrapper onto the session's target. Split out so the
// call site reads cleanly; errors flow up to be logged, never fatal.
func (s *Server) srvStageJobScript(ctx context.Context, sess *session.Session, home string) error {
	return s.driver.StageJobScript(ctx, sess, home)
}

// listTargetJobs stages the wrapper (if needed) and returns the live job registry for
// the session's dir, most-recent last (spawner-job list order). Errors mean "no jobs
// reachable" and come back as an empty slice, never a failure — the callers speak a
// friendly line either way.
func (s *Server) listTargetJobs(ctx context.Context, sess *session.Session) []bgjob.Record {
	home := session.HostHome()
	script := session.JobScriptPath(home)
	_ = s.srvStageJobScript(ctx, sess, home)
	out, err := s.driver.RunOnTarget(ctx, sess, shellQuoteArg(script)+" list --json")
	if err != nil {
		return nil
	}
	recs, err := bgjob.ParseList(trimToJSONArray(out))
	if err != nil {
		return nil
	}
	return recs
}

// doListJobs speaks the attached session's detached background jobs, numbered so the
// user can "kill job N". Running vs finished is called out per job.
func (c *conn) doListJobs() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 8*time.Second)
	defer cancel()
	recs := c.srv.listTargetJobs(ctx, c.attached)
	if len(recs) == 0 {
		c.send(msgSay("no background jobs."))
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d background %s. ", len(recs), plural(len(recs), "job", "jobs"))
	for i, r := range recs {
		state := "running"
		if r.Done {
			state = "finished"
		}
		fmt.Fprintf(&b, "%d, %s, %s. ", i+1, state, logField(r.Cmd))
	}
	b.WriteString("say kill job and a number to stop one.")
	c.send(msgSay(b.String()))
}

// doJobStatus speaks a one-line summary of the attached session's background jobs
// (how many running / finished) — the quick check versus the full doListJobs listing.
func (c *conn) doJobStatus() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 8*time.Second)
	defer cancel()
	recs := c.srv.listTargetJobs(ctx, c.attached)
	if len(recs) == 0 {
		c.send(msgSay("no background jobs."))
		return
	}
	running, done := 0, 0
	for _, r := range recs {
		if r.Done {
			done++
		} else {
			running++
		}
	}
	c.send(msgSay(fmt.Sprintf("%d running, %d finished. say list jobs for details.", running, done)))
}

// doKillJob terminates the n-th background job (1-based, matching doListJobs) of the
// attached session, taking its whole process group down via the wrapper's kill.
func (c *conn) doKillJob(n int) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	if n < 1 {
		c.send(msgSay("which job? say kill job and a number."))
		return
	}
	ctx, cancel := context.WithTimeout(c.ctx, 8*time.Second)
	defer cancel()
	recs := c.srv.listTargetJobs(ctx, c.attached)
	if n > len(recs) {
		c.send(msgSay(fmt.Sprintf("there's no job %d. say list jobs to see them.", n)))
		return
	}
	target := recs[n-1]
	home := session.HostHome()
	script := session.JobScriptPath(home)
	if _, err := c.srv.driver.RunOnTarget(ctx, c.attached, shellQuoteArg(script)+" kill "+shellQuoteArg(target.ID)); err != nil {
		c.send(msgSay("couldn't kill that job."))
		return
	}
	// Drop it from the server's mirror so a stale entry doesn't linger.
	c.attached.Jobs = dropJobByID(c.attached.Jobs, target.ID)
	if err := c.srv.store.Put(c.attached); err != nil {
		log.Printf("bgjobs[%s]: persist after kill: %v", c.attached.Name, err)
	}
	c.send(msgSay(fmt.Sprintf("killed job %d, %s.", n, logField(target.Cmd))))
}

// plural picks the singular or plural noun for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// jobNote frames a finished job's command + bounded log tail as a note Claude reads
// on the next turn. Kept compact and clearly server-authored.
func jobNote(cmd, tail string) string {
	var b strings.Builder
	b.WriteString("• `")
	b.WriteString(logField(cmd))
	b.WriteString("` finished.")
	if tail != "" {
		b.WriteString(" Last output:\n")
		b.WriteString(tail)
	}
	return b.String()
}

// capTail bounds a job log tail to jobTailMaxLines lines and jobNoteMaxRunes runes so
// one note can't dominate the turn — mirrors diffSummary's line cap and logField's
// rune cap.
func capTail(s string) string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > jobTailMaxLines {
		lines = append([]string{"… (earlier output trimmed)"}, lines[len(lines)-jobTailMaxLines:]...)
		s = strings.Join(lines, "\n")
	} else {
		s = strings.Join(lines, "\n")
	}
	r := []rune(s)
	if len(r) > jobNoteMaxRunes {
		return "…" + string(r[len(r)-jobNoteMaxRunes:])
	}
	return s
}

// dropJobByID returns jobs without the entry whose ID is id.
func dropJobByID(jobs []session.BackgroundJob, id string) []session.BackgroundJob {
	out := jobs[:0]
	for _, j := range jobs {
		if j.ID != id {
			out = append(out, j)
		}
	}
	return out
}

// trimToJSONArray isolates the JSON array in a command's combined output (the
// sandbox/SSH paths fold stderr into stdout, so a stray warning line could precede
// the array). Returns the original bytes if no array is found.
func trimToJSONArray(b []byte) []byte {
	i := strings.IndexByte(string(b), '[')
	j := strings.LastIndexByte(string(b), ']')
	if i >= 0 && j >= i {
		return b[i : j+1]
	}
	return b
}

// shellQuoteArg single-quotes a token for a POSIX-sh command line the server builds
// for RunOnTarget (paths, job ids). Mirrors the session package's shellQuote (not
// exported); kept local so the gateway needn't reach into session internals.
func shellQuoteArg(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// jobNotesPreamble frames pending background-job completion notes as leading context
// ahead of the user's dictation, so Claude treats them as status updates (a job it
// started earlier finished) rather than as new instructions. Parallels seedPreamble.
const (
	jobNotesOpen  = "[Background jobs you started earlier have finished:]\n\n"
	jobNotesClose = "\n\n[End of background-job updates. The user's message follows.]\n\n"
)

func jobNotesPreamble(notes []string) string {
	return jobNotesOpen + strings.Join(notes, "\n") + jobNotesClose
}

// jobsInstruction primes Claude (once per context) to route long-running commands
// through the spawner-job wrapper instead of run_in_background, which can't span
// turns. scriptPath is the exact staged path so Claude calls it directly (no PATH).
func jobsInstruction(scriptPath string) string {
	return "\n\n[Background jobs] For any command that should keep running after this turn ends " +
		"(long builds, servers, watches), start it with `" + scriptPath + " start '<cmd>'` instead of " +
		"run_in_background; it survives turns and you'll be told when it finishes. Check status with `" +
		scriptPath + " list`."
}

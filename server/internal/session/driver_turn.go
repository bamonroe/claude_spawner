package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// ToolUse, Usage and RateLimit are the backend-neutral turn vocabulary, owned
// by the agent package (where the per-backend parsers that produce them live).
// Aliased here so the rest of the server keeps saying session.Usage etc.
type (
	ToolUse    = agent.ToolUse
	Usage      = agent.Usage
	RateLimit  = agent.RateLimit
	TurnResult = agent.TurnResult
)

// Turn sends one user message to the session and returns the assistant's final
// prose (the stream-json `result` event). onTool, if non-nil, is called for each
// tool Claude uses, so the caller can show activity ("thinking…", "editing
// foo.go") separately from the answer. onText, if non-nil, is called with each
// assistant text message as it streams in (a whole message per call — we don't
// request token deltas), so the caller can show Claude's prose live instead of
// waiting for the whole turn to finish. onRateLimit, if non-nil, is called with
// the subscription usage-window state when the stream's rate_limit_event lands
// (early in the turn), so the caller can show the plan's session limit.
//
// The first turn (s.Started == false) creates the session with --session-id;
// later turns reattach with --resume. Turn flips s.Started to true on success —
// the caller is responsible for persisting the updated record.
func (d *Driver) Turn(ctx context.Context, s *Session, prompt string, onTool func(ToolUse), onText func(string), onRateLimit func(RateLimit)) (TurnResult, error) {
	// Every field READ in this turn comes from one snapshot taken under the record's
	// lock: a device's read loop can rename the session or change its model/agent
	// while this turn builds its command line, and a half-read record would launch
	// the wrong backend. Writes below still go to the live record via Mutate.
	snap := s.Snapshot()
	if snap.SessionID == "" {
		return TurnResult{}, fmt.Errorf("session %q has no SessionID", snap.Name)
	}
	// The session's AI backend owns the command line and the output parsing: it
	// turns this spec into the concrete flags (Claude's
	// -p/--output-format/--session-id/--model, another backend's equivalents) and
	// its ParseTurn reads the stream back. An empty/unknown Agent id resolves to
	// the default.
	ag := d.agents().Resolve(snap.Agent)
	if ag.ParseTurn == nil {
		return TurnResult{}, fmt.Errorf("agent %q has no turn parser", ag.ID)
	}
	args := ag.Args(agent.TurnSpec{
		Prompt:    prompt,
		SessionID: snap.SessionID,
		Resume:    snap.Started,
		Model:     snap.Model,
		Bypass:    d.Bypass,
		// The session's working directory. Most backends inherit it as the process
		// cwd (set by the Executor) and ignore this; Antigravity ignores cwd and
		// needs it passed explicitly (--add-dir), so it reads TurnSpec.Dir.
		Dir: snap.Dir,
		// Install the PreToolUse hook that blocks background bash and redirects it to
		// spawner-job (only the Claude backend consumes this). The wrapper is staged at
		// this same home before the turn (reconcileJobs → StageJobScript); if staging
		// failed the hook path is simply absent and Claude Code treats it as a
		// non-blocking miss, degrading to the priming-instruction behaviour.
		SettingsJSON: HookSettingsJSON(HostHome(), snap.SessionID),
		// Operator context-trimming flags (SPAWNER_CLAUDE_EXTRA_ARGS); empty by default.
		ExtraArgs: d.ClaudeExtraArgs,
	})

	// Launch via the session's execution target (host by default). The executor
	// owns process-group/abort semantics; Turn only builds args and parses stdout.
	// The backend command (claude/codex) is resolved from the agent; "" lets the
	// executor use its own configured binary (the Claude path).
	p, err := d.ProfileFor(s)
	if err != nil {
		return TurnResult{}, err
	}
	proc, err := d.executor(snap.Target).Start(ctx, snap, p, d.binFor(ag, snap.Target), args)
	if err != nil {
		return TurnResult{}, err
	}

	// For a backend that takes a caller-supplied id (Claude), the session now
	// exists on disk the moment the process launched with --session-id. Flip
	// Started here — NOT after a clean Wait — so a turn interrupted mid-stream
	// (client drop, container restart) still records that the id exists; otherwise
	// the next turn re-runs --session-id on an id claude already owns, exiting
	// status 1 forever and bricking the session. A self-assigning backend (Codex)
	// has no id yet — it's adopted from the TurnResult below and Started flips
	// then. The caller persists this even on the error path (see gateway/jobs.go).
	if !ag.SelfAssignsID {
		s.Mutate(func(s *Session) { s.Started = true })
	}

	// The agent owns its output shape: ParseTurn reads the stream into the clean
	// reply + usage. No backend branching here — a new backend brings its own
	// parser and this caller doesn't change.
	res, perr := ag.ParseTurn(proc.Stdout(), agent.TurnCallbacks{
		OnTool:      onTool,
		OnText:      onText,
		OnRateLimit: onRateLimit,
	})
	// A self-assigning backend announces its session id in the stream. Adopt it
	// and mark the session live so the next turn resumes it. Parsers return it
	// even on the error path (the id event precedes any failure), and the caller
	// persists s regardless — so a first turn that fails mid-way is still
	// resumable rather than re-created.
	if res.SessionID != "" {
		s.AdoptSessionID(res.SessionID)
	}
	if werr := proc.Wait(); werr != nil {
		return TurnResult{}, withStderr(fmt.Errorf("%s exited: %w", ag.ID, werr), proc.Stderr())
	}
	if perr != nil {
		// "stream ended without a text message" on its own tells the user nothing
		// actionable — the real cause (provider unreachable, bad model, auth) went to
		// stderr. Attach it so the chat shows why the turn failed.
		return TurnResult{}, withStderr(perr, proc.Stderr())
	}
	// An antigravity conversation id IS the name of agy's on-disk brain directory,
	// which is what the history reader replays. The generic self-assigning-backend
	// block above already adopted it as the session id; record it in the session's
	// brain chain too, so a session that spans several agy conversations (a failed
	// resume makes agy mint a fresh one) replays all of them. Skip a repeat of the
	// last id so the chain stays one entry per conversation. The caller persists s.
	if ag.Transcript == agent.TranscriptAntigravity && res.SessionID != "" {
		s.Mutate(func(s *Session) {
			if len(s.AgyBrainIDs) == 0 || s.AgyBrainIDs[len(s.AgyBrainIDs)-1] != res.SessionID {
				s.AgyBrainIDs = append(s.AgyBrainIDs, res.SessionID)
			}
		})
	}
	return res, nil
}

// Usage runs `claude -p "/usage"` headless and returns its report text (the
// stream-json `result`) — the same session/weekly percent-used breakdown the TUI
// `/usage` command shows. It is account-global (no session_id/dir), so it runs in
// a temp dir. This is a real, if lightweight, claude invocation, so callers should
// treat it as on-demand rather than per-turn.
func (d *Driver) Usage(ctx context.Context) (string, error) {
	// Give the probe an explicit session_id so we can delete its transcript once
	// it's done. Without this, every /usage run leaves a stray transcript under
	// ~/.claude/projects that session discovery surfaces as a phantom session
	// rooted at UsageDir (the first spawn root, e.g. a "/data" session that
	// reappears after the user deletes it, since deleting the store record leaves
	// the transcript on disk for the next probe to re-surface).
	id, err := NewSessionID()
	if err != nil {
		return "", err
	}
	args := []string{"-p", "/usage", "--session-id", id, "--output-format", "stream-json", "--verbose"}
	if d.Bypass {
		args = append(args, "--dangerously-skip-permissions")
	}
	// Same operator context-trimming flags as a real turn (SPAWNER_CLAUDE_EXTRA_ARGS).
	args = append(args, d.ClaudeExtraArgs...)
	// Account-global (no session_id/dir), so always run on the host — never inside
	// a per-session sandbox. UsageDir must be a jail-allowed root in broker mode;
	// fall back to a temp dir for native installs (no jail). The probe runs in its
	// own subdirectory of that base, which is what makes a leaked probe transcript
	// unmistakable rather than merely reaped — see ensureUsageProbeDir.
	base := d.UsageDir
	if base == "" {
		base = os.TempDir()
	}
	dir := d.ensureUsageProbeDir(ctx, base)
	// Reap the probe's own transcript regardless of how the run turns out, so it
	// never lingers in discovery. Only this exact session_id is removed — a real
	// session sharing UsageDir keeps its own transcript. WithoutCancel so cleanup
	// still runs when the request context is already done.
	defer func() {
		if _, derr := d.DeleteSessionByIDs(context.WithoutCancel(ctx), LocalHost, []string{id}); derr != nil {
			log.Printf("usage: delete probe transcript %s: %v", id, derr)
		}
	}()
	// Account-global probe: run it on the loopback host explicitly (the SSH executor
	// no longer defaults a hostless session). A purely remote deployment with no
	// reachable local box can't run /usage; that's an accepted limitation.
	proc, err := d.executor(TargetHost).Start(ctx, &Session{Name: "usage", Dir: dir, Host: LocalHost}, nil, "", args)
	if err != nil {
		return "", err
	}
	// The probe is inherently Claude-specific (the /usage slash command and the
	// hand-built args above), so parse with the claude agent's parser explicitly.
	res, perr := d.agents().Resolve("claude").ParseTurn(proc.Stdout(), agent.TurnCallbacks{})
	if werr := proc.Wait(); werr != nil {
		return "", fmt.Errorf("claude exited: %w", werr)
	}
	if perr != nil {
		return "", perr
	}
	return res.Reply, nil
}

// usageProbeSubdir is the dedicated working directory the /usage probe runs in,
// created beneath Driver.UsageDir. The probe's transcript lands in
// ~/.claude/projects under an encoding of its cwd, and discovery surfaces every
// transcript it finds there — so with the probe running straight in $HOME, any
// transcript that outlived its reap came back as a phantom session named after
// the home directory's basename, indistinguishable from a real one.
//
// Its own directory makes the leak IDENTIFIABLE rather than merely rare:
// isUsageProbeDir recognizes it by cwd and discovery drops it, so a probe
// transcript can never masquerade as a session even if the reap fails outright.
// The reap in Driver.Usage still runs — this is the second line, not a
// replacement for the first.
const usageProbeSubdir = ".spawner-usage"

// isUsageProbeDir reports whether a discovered transcript's working directory is
// a /usage probe dir. Suffix match rather than equality against UsageDir: this
// same check runs over transcripts discovered on ANY host, whose home directory
// (and therefore whose UsageDir) this process doesn't know. The path separator is
// the remote's, i.e. always POSIX.
func isUsageProbeDir(dir string) bool {
	return strings.HasSuffix(dir, "/"+usageProbeSubdir)
}

// ensureUsageProbeDir creates the probe's own directory beneath base ON THE HOST
// (the probe runs there over SSH, so the server container's own filesystem is the
// wrong one to create it in) and returns it.
//
// A failure falls back to base, which is exactly today's behavior: /usage keeps
// working and only loses the discovery-proof isolation for that run. Making the
// probe hard-depend on the mkdir would trade a cosmetic phantom for a broken
// feature.
func (d *Driver) ensureUsageProbeDir(ctx context.Context, base string) string {
	probe := filepath.Join(base, usageProbeSubdir)
	if pool := d.hostPool(); pool != nil {
		if err := pool.MakeDir(ctx, LocalHost, probe); err != nil {
			log.Printf("usage: create probe dir %s: %v (falling back to %s)", probe, err, base)
			return base
		}
		return probe
	}
	// No SSH pool means the test-only HostExecutor: the host IS this process.
	if err := os.MkdirAll(probe, 0o755); err != nil {
		log.Printf("usage: create probe dir %s: %v (falling back to %s)", probe, err, base)
		return base
	}
	return probe
}

// stderrCauseMax caps how much captured stderr is appended to a turn error. The
// error becomes a chat bubble read aloud on a phone, so it keeps the tail (where
// the cause is) short enough to stay readable.
const stderrCauseMax = 600

// withStderr appends the process's captured stderr to a turn error as the
// actionable cause. A backend that dies without a usable stream ("stream ended
// without a text message", "exited: status 1") says nothing on its own — stderr
// is where "model not found" or "connection refused" actually is — so the two are
// reported together, or err is returned unchanged when stderr was empty.
func withStderr(err error, stderr string) error {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return err
	}
	if len(stderr) > stderrCauseMax {
		stderr = "…" + stderr[len(stderr)-stderrCauseMax:]
	}
	return fmt.Errorf("%w: %s", err, stderr)
}

package session

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// ToolUse, Usage and RateLimit are the backend-neutral turn vocabulary, owned
// by the agent package (where the per-backend parsers that produce them live).
// Aliased here so the rest of the server keeps saying session.Usage etc.
type (
	ToolUse   = agent.ToolUse
	Usage     = agent.Usage
	RateLimit = agent.RateLimit
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
func (d *Driver) Turn(ctx context.Context, s *Session, prompt string, onTool func(ToolUse), onText func(string), onRateLimit func(RateLimit)) (string, Usage, error) {
	if s.SessionID == "" {
		return "", Usage{}, fmt.Errorf("session %q has no SessionID", s.Name)
	}
	// The session's AI backend owns the command line and the output parsing: it
	// turns this spec into the concrete flags (Claude's
	// -p/--output-format/--session-id/--model, another backend's equivalents) and
	// its ParseTurn reads the stream back. An empty/unknown Agent id resolves to
	// the default.
	ag := d.agents().Resolve(s.Agent)
	if ag.ParseTurn == nil {
		return "", Usage{}, fmt.Errorf("agent %q has no turn parser", ag.ID)
	}
	args := ag.Args(agent.TurnSpec{
		Prompt:    prompt,
		SessionID: s.SessionID,
		Resume:    s.Started,
		Model:     s.Model,
		Bypass:    d.Bypass,
		// The session's working directory. Most backends inherit it as the process
		// cwd (set by the Executor) and ignore this; Antigravity ignores cwd and
		// needs it passed explicitly (--add-dir), so it reads TurnSpec.Dir.
		Dir: s.Dir,
		// Install the PreToolUse hook that blocks background bash and redirects it to
		// spawner-job (only the Claude backend consumes this). The wrapper is staged at
		// this same home before the turn (reconcileJobs → StageJobScript); if staging
		// failed the hook path is simply absent and Claude Code treats it as a
		// non-blocking miss, degrading to the priming-instruction behaviour.
		SettingsJSON: HookSettingsJSON(HostHome(), s.SessionID),
	})

	// Launch via the session's execution target (host by default). The executor
	// owns process-group/abort semantics; Turn only builds args and parses stdout.
	// The backend command (claude/codex) is resolved from the agent; "" lets the
	// executor use its own configured binary (the Claude path).
	p, err := d.ProfileFor(s)
	if err != nil {
		return "", Usage{}, err
	}
	s.ResolvedProfile = p
	proc, err := d.executor(s.Target).Start(ctx, s, d.binFor(ag, s.Target), args)
	if err != nil {
		return "", Usage{}, err
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
		s.Started = true
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
		s.SessionID = res.SessionID
		s.Started = true
	}
	if werr := proc.Wait(); werr != nil {
		return "", Usage{}, fmt.Errorf("%s exited: %w", ag.ID, werr)
	}
	if perr != nil {
		return "", Usage{}, perr
	}
	// Antigravity's stdout collapses a turn's several messages into one blank-line-less
	// blob; rebuild the paragraph breaks from agy's on-disk transcript (best-effort —
	// falls back to the stdout reply on any miss). See antigravity_transcript.go.
	if ag.Transcript == agent.TranscriptAntigravity {
		var brainID string
		res.Reply, brainID = d.reconstructAgyReply(ctx, s, res.Reply)
		// Record the brain dir this turn wrote (when we could pin it down) so the
		// history reader can later replay the session's turns — agy won't let us find
		// them by our own id. Skip a repeat of the last id (a retry matching the same
		// dir) so the chain stays one entry per turn. The caller persists s.
		if brainID != "" && (len(s.AgyBrainIDs) == 0 || s.AgyBrainIDs[len(s.AgyBrainIDs)-1] != brainID) {
			s.AgyBrainIDs = append(s.AgyBrainIDs, brainID)
		}
	}
	return res.Reply, res.Usage, nil
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
	// Account-global (no session_id/dir), so always run on the host — never inside
	// a per-session sandbox. UsageDir must be a jail-allowed root in broker mode;
	// fall back to a temp dir for native installs (no jail).
	dir := d.UsageDir
	if dir == "" {
		dir = os.TempDir()
	}
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
	proc, err := d.executor(TargetHost).Start(ctx, &Session{Name: "usage", Dir: dir, Host: LocalHost}, "", args)
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

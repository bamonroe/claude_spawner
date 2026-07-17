// Package session drives headless AI backend turns and tracks sessions as
// durable records (a session_id on disk tied to a directory), not live
// processes. This is the data path for the voice interface: each dictated turn
// shells out to the session's backend CLI (Claude Code's `claude -p
// --output-format stream-json` by default — see internal/agent for the backend
// registry) and the clean reply text is returned for text-to-speech. See
// docs/protocol.md and the "TUI capture" decision in CLAUDE.md.
package session

import (
	"context"
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// newLineScanner is the JSONL scanner shared by the transcript/discover
// readers; it lives in the agent package alongside the backend stream parsers
// that also use it.
var newLineScanner = agent.NewLineScanner

// Session is a durable record. There is no long-lived process: the conversation
// state lives on disk under SessionID and is reattached via `claude --resume`.
type Session struct {
	Name      string `json:"name"`       // human/voice handle, e.g. "claude-claude"
	Dir       string `json:"dir"`        // working directory for the session
	SessionID string `json:"session_id"` // claude session uuid (generated at spawn)
	Started   bool   `json:"started"`    // false until the first turn has run
	// AskPrimed records that the interactive-mode ask instruction has been sent to
	// Claude for the current context, so later turns don't re-append it (Claude
	// keeps it via --resume). Reset by "clear"/"compress", which rotate the context.
	AskPrimed bool `json:"ask_primed,omitempty"`
	// PriorIDs are session_ids retired by "clear"/"compress" (context rotation),
	// oldest first. Their transcripts stay on disk so the app can show the full
	// history, but Claude only ever resumes the current SessionID — so a rotation
	// changes context without losing (or re-reading) the record.
	PriorIDs []string `json:"prior_ids,omitempty"`
	// PendingSeed is a condensed summary of the prior context, produced by
	// "compress" when it rotated the session_id. It is prepended to the FIRST
	// dictation on the fresh SessionID (so Claude continues with the compacted
	// context) and then cleared. Empty except in the window between a compress and
	// the next dictation. "clear" wipes it (a clear means truly empty context).
	PendingSeed string `json:"pending_seed,omitempty"`
	// Target selects where this session's turns run: TargetHost (direct exec on the
	// host — real host files/toolchains) or TargetSandbox (an isolated container).
	// Chosen at spawn time and durable. Empty means host (records predate this
	// field). Turn resolves it to a registered Executor. See docs/architecture.md.
	Target Target `json:"target,omitempty"`
	// Container is the name of the persistent sandbox container bound to this
	// session's lifetime (sandbox target only): created at spawn, reused every turn,
	// removed on delete. Empty for host sessions.
	Container string `json:"container,omitempty"`
	// Host is the SSH target where this session's turns run under SSH-native
	// execution: empty means the local machine (loopback), a name like "work" means
	// that remote box. SSHExecutor reads it to pick the pooled connection. Reserved:
	// the spawn-dialog choice and Driver routing that select the SSH executor land in
	// a later commit of the SSH-native epic (see TODO.md); today nothing sets it.
	Host string `json:"host,omitempty"`
	// Agent is the id of the AI backend this session runs (agent.Registry). Empty
	// means the default backend — records predate this field, and it keeps old
	// sessions on Claude. Chosen at spawn time and durable. Turn resolves it to a
	// registered agent.Agent, which builds the turn's command line.
	Agent string `json:"agent,omitempty"`
	// Model is the backend model alias for this session (e.g. "opus", "sonnet",
	// "fable"). Empty means the backend's own configured default (no --model flag);
	// spawn stamps the agent's DefaultModel here, and a voice command can change it.
	Model string `json:"model,omitempty"`
	// Profile is the execution-environment profile name. Empty means the built-in
	// default profile, preserving records written before profiles existed.
	Profile string `json:"profile,omitempty"`
	// ResolvedProfile is set by Driver immediately before launching a turn or
	// sandbox lifecycle command. It is deliberately not persisted.
	ResolvedProfile *ExecProfile `json:"-"`
	// Jobs mirrors the detached background jobs Claude launched for this session via
	// the spawner-job wrapper (see internal/session/bgjob). The reconciler diffs the
	// on-target registry against this list at turn boundaries; a job that just
	// finished has its log tail injected into the next turn and is marked Notified.
	// Because the on-target registry is keyed by Dir (not session_id), Jobs ride the
	// struct through clear/compress session_id rotation and MUST NOT be wiped by a
	// context clear — a background job outlives a context reset.
	Jobs []BackgroundJob `json:"jobs,omitempty"`
	// PendingNotes are framed completion notes for finished background jobs, waiting
	// to be prepended to the next dictation so Claude learns a job it started earlier
	// has finished (with a bounded log tail). Cleared once injected. Like Jobs, this
	// survives a context clear.
	PendingNotes []string `json:"pending_notes,omitempty"`
	// JobsPrimed records that the background-job instruction (use spawner-job for
	// long-running commands) has been sent to Claude for the current context, so it
	// isn't re-appended every turn. Reset by clear/compress like AskPrimed
	// (re-priming after a context rotation is harmless).
	JobsPrimed bool `json:"jobs_primed,omitempty"`
	// AgyBrainIDs records, in turn order, the antigravity "brain" directory id each
	// turn wrote under ~/.gemini/antigravity-cli/brain/<id>/. Unlike Claude/Codex,
	// agy IGNORES the --conversation id we pass and files every turn under a fresh
	// internal id of its own, so we can't recover a session's history by our own
	// session_id. Instead we capture the brain id when we locate a turn's transcript
	// to rebuild its reply (reconstructAgyReply), building our own ordered map from
	// this session to agy's on-disk turns — the antigravity history reader replays
	// these. Empty for non-antigravity sessions. Like Jobs, it rides through the
	// clear/compress session_id rotation (its transcripts stay on disk for scrollback).
	AgyBrainIDs []string `json:"agy_brain_ids,omitempty"`
}

// BackgroundJob is one detached job Claude launched via the spawner-job wrapper,
// as tracked by the server across turns. The authoritative live state is the
// on-target registry (keyed by Dir); this is the server's view of which jobs it has
// already told Claude about, so a finished job is announced exactly once.
type BackgroundJob struct {
	ID       string `json:"id"`                  // spawner-job registry id (epoch_pid_rand)
	Cmd      string `json:"cmd"`                 // the shell command it runs
	Started  int64  `json:"started"`             // epoch seconds it was launched
	Done     bool   `json:"done"`                // observed finished by the reconciler
	ExitCode int    `json:"exit_code,omitempty"` // best-effort (detached jobs report 0)
	Notified bool   `json:"notified"`            // its completion note was injected already
}

// TranscriptIDs returns every session_id whose transcript belongs to this
// session, oldest first: ids retired by "clear" followed by the current one.
// Used to assemble the full history for display without Claude re-reading it.
func (s *Session) TranscriptIDs() []string {
	ids := make([]string, 0, len(s.PriorIDs)+1)
	ids = append(ids, s.PriorIDs...)
	ids = append(ids, s.SessionID)
	return ids
}

// HasPriorID reports whether id is one of the session_ids this session retired via
// a "clear"/"compress" context rotation (see PriorIDs). It does NOT match the
// current SessionID — callers check that separately.
func (s *Session) HasPriorID(id string) bool {
	for _, prior := range s.PriorIDs {
		if prior == id {
			return true
		}
	}
	return false
}

// Driver runs Claude Code turns. It holds no per-session state.
type Driver struct {
	// Execs maps an execution Target to the Executor that launches its turns. Turn
	// and Usage select by the session's Target (empty/unknown falls back to
	// TargetHost, which must always be registered). Register a sandbox target to
	// make host-vs-container a per-session choice.
	Execs map[Target]Executor
	// Agents is the registry of AI backends. Turn resolves a session's Agent id
	// here to build the turn's command line and pick the output parser; an empty or
	// unknown id falls back to the registry's default backend (Claude). Never nil
	// after NewDriver.
	Agents *agent.Registry
	// AgentBins overrides a backend's binary per (agent id, target) from config —
	// e.g. {"codex": {host: SPAWNER_SSH_CODEX_BIN, sandbox: SPAWNER_SANDBOX_CODEX_BIN}}.
	// A non-empty entry wins for that target; a missing/empty one falls through to
	// the agent's own Bin (then the executor's per-target config). Claude is absent
	// here — it defers to each executor's Bin (SPAWNER_CLAUDE_BIN /
	// SPAWNER_SANDBOX_CLAUDE_BIN / SPAWNER_SSH_CLAUDE_BIN), so its wiring is
	// unchanged. (SSH reuses the host target, so its Codex bin is wired into the
	// host entry when SSH is enabled — see main.go.) Nil is fine (no overrides).
	AgentBins map[string]map[Target]string
	// Profiles is the execution-environment profile registry. Empty/unknown
	// session profile names resolve to the built-in default profile.
	Profiles *ProfileRegistry
	// Providers is the app-managed per-backend settings overlay (default model +
	// voice-enumerable model subset). Nil is fine: every read falls back to the
	// backend's compiled defaults (the store's methods are nil-safe).
	Providers *agent.SettingsStore
	// Home is the {{.Home}} template value — the login user's home on the
	// executing host. GlobalVars are the server-wide {{.Vars.X}} values a profile's
	// own vars overlay. Both feed profile templating in ProfileFor.
	Home       string
	GlobalVars map[string]string
	// Bypass adds --dangerously-skip-permissions when true (project default).
	Bypass bool
	// UsageDir is the working directory for the account-global /usage check. It has
	// no session on disk, so any directory works; empty falls back to os.TempDir().
	UsageDir string
	// RestartCmd is the shell command (run via `sh -c`, detached) that rebuilds and
	// relaunches the server for the app's "restart" button. Empty disables restart.
	// See Driver.Restart.
	RestartCmd string
}

// NewDriver returns a Driver with project defaults: a single host executor
// running the "claude" binary, --dangerously-skip-permissions on. Use HostBin to
// point it at a different binary, and register more entries in Execs for other
// targets.
func NewDriver() *Driver {
	return &Driver{
		Execs:  map[Target]Executor{TargetHost: HostExecutor{Bin: "claude"}},
		Agents: agent.Default(),
		Bypass: true,
	}
}

// HostBin points the host executor at a specific claude binary. Convenience for
// wiring (config's SPAWNER_CLAUDE_BIN) and tests; equivalent to replacing
// Execs[TargetHost].
func (d *Driver) HostBin(bin string) { d.Execs[TargetHost] = HostExecutor{Bin: bin} }

// SandboxEnabled reports whether the sandbox target is available (an executor is
// registered for it), so the spawn flow only offers "host or sandbox?" when
// sandbox sessions can actually run.
func (d *Driver) SandboxEnabled() bool {
	_, ok := d.Execs[TargetSandbox]
	return ok
}

// agents returns the driver's backend registry, defaulting to the built-in
// registry when unset so a Driver built as a literal (tests, minimal callers)
// still resolves a session's agent.
func (d *Driver) agents() *agent.Registry {
	if d.Agents == nil {
		d.Agents = agent.Default()
	}
	return d.Agents
}

// binFor resolves the backend command to launch for a session's agent on a
// target. A per-target AgentBins config override wins; otherwise the agent's own
// Bin is used. Claude's Bin is empty and it has no AgentBins entry, so it returns
// "" — the Executor then uses its own configured binary, preserving the
// pre-registry behavior on every target.
func (d *Driver) binFor(ag *agent.Agent, t Target) string {
	if m := d.AgentBins[ag.ID]; m != nil {
		if b := m[t]; b != "" {
			return b
		}
	}
	return ag.Bin
}

// AgentFor resolves the AI backend a session runs on (its Agent id, empty/unknown
// → the default backend). Exposed so the gateway can read a session's model
// catalogue for the "list models" / "use model N" voice commands.
func (d *Driver) AgentFor(s *Session) *agent.Agent { return d.agents().Resolve(s.Agent) }

// Agents returns the backend registry (never nil), so the gateway can resolve a
// named backend at spawn and list the available backends.
func (d *Driver) Registry() *agent.Registry { return d.agents() }

// ProfileRegistry returns the execution-profile registry, creating a minimal
// default-only registry for tests and older callers that build Driver literals.
func (d *Driver) ProfileRegistry() *ProfileRegistry {
	if d.Profiles == nil {
		d.Profiles, _ = NewProfileRegistry(ExecProfile{Name: "bare-metal", Target: TargetHost, Default: true})
	}
	return d.Profiles
}

// ProviderSettings returns the app-managed per-backend settings overlay, creating
// an empty in-memory store (bound to the backend registry) for tests and older
// callers that build a Driver literal. The store's read methods are nil-safe, but
// mutating handlers (provider_put) need a real store, so this never returns nil.
func (d *Driver) ProviderSettings() *agent.SettingsStore {
	if d.Providers == nil {
		d.Providers, _ = agent.OpenSettingsStore("", d.agents())
	}
	return d.Providers
}

// ProfileFor resolves the execution profile a session uses and renders its
// {{.Var}} templates against the session + global context. A template referencing
// an undefined var is a hard error, surfaced to the caller (and thus the turn).
func (d *Driver) ProfileFor(s *Session) (*ExecProfile, error) {
	name := ""
	ctx := RenderContext{Home: d.Home}
	if s != nil {
		name = s.Profile
		ctx.Session = s.Name
		ctx.Dir = s.Dir
	}
	p := d.ProfileRegistry().Resolve(name)
	ctx.Vars = mergeVars(d.GlobalVars, p.Vars)
	rendered, err := p.render(ctx)
	if err != nil {
		return nil, fmt.Errorf("profile %q: %w", p.Name, err)
	}
	return rendered, nil
}

// executor resolves a Target to its Executor, falling back to the host executor
// for the empty string or any target with no registered executor.
func (d *Driver) executor(t Target) Executor {
	if t != "" {
		if e, ok := d.Execs[t]; ok {
			return e
		}
	}
	return d.Execs[TargetHost]
}

// EnsureContainer creates the session's persistent sandbox container if it isn't
// already running (called at spawn). A no-op for host sessions, or when the
// sandbox executor isn't registered / has no lifecycle support.
func (d *Driver) EnsureContainer(ctx context.Context, s *Session) error {
	if s.Target != TargetSandbox || s.Container == "" {
		return nil
	}
	if lc, ok := d.Execs[TargetSandbox].(SandboxLifecycle); ok {
		p, err := d.ProfileFor(s)
		if err != nil {
			return err
		}
		s.ResolvedProfile = p
		return lc.Ensure(ctx, s)
	}
	return nil
}

// RemoveContainer destroys the session's persistent sandbox container (called on
// delete). A no-op for host sessions or when there's no sandbox lifecycle.
func (d *Driver) RemoveContainer(ctx context.Context, s *Session) error {
	if s.Target != TargetSandbox || s.Container == "" {
		return nil
	}
	if lc, ok := d.Execs[TargetSandbox].(SandboxLifecycle); ok {
		return lc.Remove(ctx, s.Container)
	}
	return nil
}

// ReconcileContainers sweeps orphaned sandbox containers at startup: any managed
// container whose name isn't in `known` (the set of container names still owned by
// live session records) is removed — it belonged to a session deleted while the
// server was down. Returns the names removed. A no-op when the sandbox executor
// can't list its containers.
func (d *Driver) ReconcileContainers(ctx context.Context, known map[string]bool) ([]string, error) {
	reaper, ok := d.Execs[TargetSandbox].(SandboxReaper)
	if !ok {
		return nil, nil
	}
	names, err := reaper.List(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, n := range names {
		if known[n] {
			continue
		}
		if err := reaper.Remove(ctx, n); err != nil {
			return removed, fmt.Errorf("remove orphan %s: %w", n, err)
		}
		removed = append(removed, n)
	}
	return removed, nil
}

// Restart fires the configured RestartCmd to rebuild and relaunch the server (the
// app's "restart" button). When a host SSH pool is configured the command runs on
// the host over that Go-native connection; otherwise it runs locally, detached in
// its own process group via `sh -c`. Either way the command `setsid`s the rebuild
// so it survives the server's own termination when the container is recreated. It
// returns once the rebuild is LAUNCHED — the process is replaced moments later — or
// an error if restart isn't configured. Errors from the command are logged, not
// returned.
// Restart fires the configured restart command. mode picks what happens: "build"
// rebuilds the image only (the running container is left in place — no bounce),
// "bounce" recreates the container from the existing image without recompiling, and
// "rebuild" (the default, empty = rebuild) builds then recreates. The command may
// contain the token `%REBUILD%`, which is replaced with the mode and forwarded to
// deploy/rebuild-container.sh as its first arg (the script builds and/or recreates
// accordingly). Commands without the token run unchanged — an older config always does
// a full rebuild.
func (d *Driver) Restart(ctx context.Context, mode string) error {
	if d.RestartCmd == "" {
		return fmt.Errorf("server restart is not configured (set SPAWNER_RESTART_CMD)")
	}
	switch mode {
	case "", "rebuild":
		mode = "rebuild"
	case "build", "bounce":
		// valid as-is
	default:
		return fmt.Errorf("unknown restart mode %q (want build, bounce, or rebuild)", mode)
	}
	cmdStr := strings.ReplaceAll(d.RestartCmd, "%REBUILD%", mode)
	// Prefer the in-process SSH pool: the rebuild must run on the HOST (it recreates
	// this very container), and the pool already reaches the host for turns — so we
	// run the command there over Go-native SSH rather than shelling to the openssh
	// client. The remote command `setsid`s the rebuild, so it stays decoupled from
	// this container even as the SSH channel dies during recreate. Using openssh here
	// was the only reason the container needed an /etc/passwd entry.
	if pool := d.hostPool(); pool != nil {
		log.Printf("restart: launching over ssh pool on %s: %q", LocalHost, cmdStr)
		go func() {
			// Background ctx: the caller's ctx dies with the request, but the rebuild
			// is already detached on the host — don't let a cancel signal the channel.
			if _, err := pool.Run(context.Background(), LocalHost, cmdStr); err != nil {
				log.Printf("restart command failed: %v", err)
			}
		}()
		return nil
	}
	// No SSH pool: only reachable from tests (production always wires the pool). Run
	// locally, detached in its own process group so the rebuild would survive the
	// server's own termination on recreate.
	cmd := exec.Command("sh", "-c", cmdStr)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("restart: launched %q", cmdStr)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("restart command failed: %v", err)
		}
	}()
	return nil
}

// hostPool returns the SSH connection pool used for host turns. In production the
// host executor is always an SSHExecutor, so this is non-nil; it returns nil only
// under the test-only HostExecutor. Restart and claudeFSFor reuse it to reach the
// host without the openssh client.
func (d *Driver) hostPool() *SSHPool {
	if ex, ok := d.Execs[TargetHost].(SSHExecutor); ok {
		return ex.Pool
	}
	return nil
}

// RefreshModels asks each backend that supports live discovery to report the
// models it can currently run, and installs the result as that backend's
// effective catalogue (agent.Agent.Catalog). Discovery runs the backend's probe
// command on the host over the SSH pool (LocalHost), using the backend's host
// binary — so, e.g., opencode's `opencode models ollama` surfaces whatever the
// host's opencode is configured for, no server rebuild needed.
//
// Best-effort and per-backend isolated: no host pool, a failing probe, or an
// empty/unparseable result each just leaves that backend on its compiled fallback
// list. Safe to call repeatedly (boot prime + periodic refresh); it never errors
// out the caller.
func (d *Driver) RefreshModels(ctx context.Context) {
	pool := d.hostPool()
	if pool == nil {
		return // test-only HostExecutor, or SSH disabled — keep compiled catalogues
	}
	for _, ag := range d.Registry().List() {
		if !ag.CanDiscover() {
			continue
		}
		cmd := shellJoinCmd(d.binFor(ag, TargetHost), ag.DiscoverArgs)
		out, err := pool.Run(ctx, LocalHost, cmd)
		if err != nil {
			log.Printf("model discovery: %s (%q): %v — keeping compiled models", ag.ID, cmd, err)
			continue
		}
		models := ag.ParseModels(out)
		if len(models) == 0 {
			log.Printf("model discovery: %s returned no models — keeping compiled models", ag.ID)
			continue
		}
		ag.SetDiscovered(models)
		log.Printf("model discovery: %s → %d model(s)", ag.ID, len(models))
	}
}

// DeleteSessionsForDir removes a directory's Claude transcripts on the session's
// host (empty host = local). Returns how many transcripts were removed.
func (d *Driver) DeleteSessionsForDir(ctx context.Context, host, sessionID, dir string) (int, error) {
	return d.claudeFSFor(host).deleteForDir(sessionID, dir)
}

// MakeSpawnDir creates a brand-new project directory for a spawn. The caller is
// expected to have jail-validated dir.
func (d *Driver) MakeSpawnDir(ctx context.Context, dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// DeleteSessionByIDs fully purges exactly the given session_ids (one logical
// session) on the session's host (empty host = local): each id's transcript, its
// sidecar dir, and its per-session state, leaving dir-mates intact. Claude-format
// only — use DeleteSession when the backend may be Codex.
func (d *Driver) DeleteSessionByIDs(ctx context.Context, host string, ids []string) (int, error) {
	return d.claudeFSFor(host).deleteByIDs(ids)
}

// DeleteSession fully purges a session's on-disk state for its backend: the
// Claude transcript + sidecar + per-session state dirs, or a Codex session's
// rollout files. ids is the session's transcript chain (current + rotated prior
// ids). host empty = local machine.
func (d *Driver) DeleteSession(agentID, host string, ids []string) (int, error) {
	return d.transcriptReaderFor(agentID, host).deleteByIDs(ids)
}

// transcriptReader reads a session's past conversation and context snapshot from
// on-disk state, and purges it on delete, for whichever backend + host the
// session runs on. claudeFS and codexFS each implement it; transcriptReaderFor
// picks by the session's backend so a Codex session's rollout replays on reattach
// (and is deleted) just like a Claude transcript.
type transcriptReader interface {
	readTranscriptChain(ids []string) ([]Message, error)
	lastContextUsage(ids []string) *ContextSnapshot
	deleteByIDs(ids []string) (int, error)
}

// transcriptReaderFor selects the on-disk reader for a session's backend (agent
// id) on its host, by the agent's declared transcript layout: Codex reads its
// rollout files, opencode shells out to its export command, every other backend
// reads Claude-style transcripts. host empty = local machine.
func (d *Driver) transcriptReaderFor(agentID, host string) transcriptReader {
	switch d.agents().Resolve(agentID).Transcript {
	case agent.TranscriptCodex:
		return codexFS{d.claudeFSFor(host)}
	case agent.TranscriptOpencode:
		return opencodeFS{d.claudeFSFor(host)}
	case agent.TranscriptAntigravity:
		// agy's on-disk store isn't wired to a reader yet (keyed by an internal id
		// we don't hold, and it records no token usage), so history replay/context/
		// deletion are backed by nothing rather than accidentally reading a
		// co-located Claude transcript. Its reply still streams live off stdout.
		return nullTranscript{}
	}
	return d.claudeFSFor(host)
}

// ReadTranscriptChain reads a session's full history (current + rotated prior ids)
// from its host (empty host = local), re-indexed contiguously for pagination.
// agentID selects the backend's on-disk format (Claude transcript vs Codex rollout).
func (d *Driver) ReadTranscriptChain(agentID, host string, ids []string) ([]Message, error) {
	return d.transcriptReaderFor(agentID, host).readTranscriptChain(ids)
}

// LastContextUsage returns a session's live context snapshot (last usage-bearing
// turn) read from its host (empty host = local); nil if none yet. agentID selects
// the backend's on-disk format.
func (d *Driver) LastContextUsage(agentID, host string, ids []string) *ContextSnapshot {
	return d.transcriptReaderFor(agentID, host).lastContextUsage(ids)
}

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
		SettingsJSON: HookSettingsJSON(HostHome()),
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

// NewContainerName returns a unique sandbox container name ("spawner-sbx-<hex>"),
// independent of the session name (which can be renamed) and the claude
// session_id (which rotates on clear/compress), so it stays valid for the
// session's whole life.
func NewContainerName() (string, error) {
	return NewContainerNameWithPrefix(containerPrefix)
}

// NewContainerNameWithPrefix is NewContainerName under a caller-supplied name
// namespace. Tests use a unique prefix so their SandboxExecutor.List/reconcile
// can only ever see (and remove) their own containers, never a real session's.
func NewContainerNameWithPrefix(prefix string) (string, error) {
	id, err := NewSessionID()
	if err != nil {
		return "", err
	}
	return prefix + strings.ReplaceAll(id, "-", "")[:12], nil
}

// NewSessionID returns a random RFC-4122 v4 UUID for use with --session-id.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

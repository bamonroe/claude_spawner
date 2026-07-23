package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/agent"
)

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

// currentHistoryIDs returns the ids under which the session's CURRENT backend
// stores its display transcripts: antigravity files under its own brain ids (agy
// ignores our session_id), every other backend under its session_id chain.
func (d *Driver) currentHistoryIDs(rec *Session) []string {
	if d.agents().Resolve(rec.Agent).Transcript == agent.TranscriptAntigravity {
		return append([]string(nil), rec.AgyBrainIDs...)
	}
	return rec.TranscriptIDs()
}

// ArchiveSegment captures the session's current backend as a display HistorySegment,
// to be appended to rec.History just before a set_agent switch rotates the backend
// away — so the outgoing backend's messages stay in the chat log even though the new
// backend won't read them as context.
func (d *Driver) ArchiveSegment(rec *Session) HistorySegment {
	return HistorySegment{Agent: rec.Agent, Host: rec.Host, IDs: d.currentHistoryIDs(rec)}
}

// ReadDisplayHistory reads a session's full cross-backend chat log for display: each
// archived HistorySegment (a previous backend) via that backend's own reader, oldest
// first, then the current backend's chain — concatenated and re-indexed contiguously
// so pagination cursors stay stable across the whole log. A failed archived segment is
// logged and skipped (best-effort scrollback); only the current backend's read fails
// the call, matching pre-split behavior. With no History this equals the old
// ReadTranscriptChain(current) exactly.
func (d *Driver) ReadDisplayHistory(rec *Session) ([]Message, error) {
	var all []Message
	for _, seg := range rec.History {
		msgs, err := d.transcriptReaderFor(seg.Agent, seg.Host).readTranscriptChain(seg.IDs)
		if err != nil {
			log.Printf("display history[%s]: read archived %s segment: %v", rec.Name, seg.Agent, err)
			continue
		}
		all = append(all, msgs...)
	}
	cur, err := d.transcriptReaderFor(rec.Agent, rec.Host).readTranscriptChain(d.currentHistoryIDs(rec))
	if err != nil {
		return nil, err
	}
	all = append(all, cur...)
	for i := range all {
		all[i].Index = i
	}
	return all, nil
}

// DeleteSessionAll purges every on-disk transcript of a session across ALL the
// backends it ran: each archived History segment via its own backend reader, plus
// the current backend's chain. Use for a full session delete so a backend switched
// away from doesn't orphan its transcripts. Returns the count removed; best-effort
// per segment (a segment error is logged, not fatal).
func (d *Driver) DeleteSessionAll(rec *Session) (int, error) {
	total := 0
	for _, seg := range rec.History {
		n, err := d.transcriptReaderFor(seg.Agent, seg.Host).deleteByIDs(seg.IDs)
		if err != nil {
			log.Printf("delete session[%s]: purge archived %s segment: %v", rec.Name, seg.Agent, err)
		}
		total += n
	}
	n, err := d.transcriptReaderFor(rec.Agent, rec.Host).deleteByIDs(d.currentHistoryIDs(rec))
	total += n
	return total, err
}

// LastContextUsage returns a session's live context snapshot (last usage-bearing
// turn) read from its host (empty host = local); nil if none yet. agentID selects
// the backend's on-disk format.
func (d *Driver) LastContextUsage(agentID, host string, ids []string) *ContextSnapshot {
	return d.transcriptReaderFor(agentID, host).lastContextUsage(ids)
}

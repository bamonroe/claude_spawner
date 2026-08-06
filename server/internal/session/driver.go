package session

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// Driver runs Claude Code turns. It holds no per-session state.
type Driver struct {
	// Execs maps an execution Target to the Executor that launches its turns. Turn
	// and Usage select by the session's Target (empty/unknown falls back to
	// TargetHost, which must always be registered). Register a sandbox target to
	// make host-vs-container a per-session choice.
	//
	// Populate it at construction (a literal, or NewDriver) and change it ONLY via
	// SetExec/HostBin afterwards: the driver is shared by every turn goroutine and
	// by background work like the job reconciler, so a bare map write once the
	// driver is live races their lookups. execsMu guards every post-construction
	// access; the internal readers below all go through exec().
	Execs   map[Target]Executor
	execsMu sync.RWMutex
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
	// ClaudeExtraArgs are operator-supplied flags (SPAWNER_CLAUDE_EXTRA_ARGS)
	// appended to every Claude turn and the /usage probe, for trimming the per-turn
	// context (e.g. --disable-slash-commands). Nil is the no-op default.
	ClaudeExtraArgs []string
	// UsageDir is the working directory for the account-global /usage check. It has
	// no session on disk, so any directory works; empty falls back to os.TempDir().
	UsageDir string
	// RestartCmd is the shell command (run via `sh -c`, detached) that rebuilds and
	// relaunches the server for the app's "restart" button. Empty disables restart.
	// See Driver.Restart.
	RestartCmd string

	// digests is the durable transcript-digest cache used by DisplayDigest. Nil is
	// fine — every digest is then recomputed from a full parse. Set via SetDigests.
	digests *DigestCache

	// usage is the durable last-context-usage cache used by SessionContextUsage.
	// Nil is fine — usage is then re-read from the transcript tail every time.
	// Set via SetUsageCache.
	usage *UsageCache

	// displayOnce/displayMemoized lazily build the in-memory display-history memo,
	// so a Driver built as a struct literal (tests, minimal callers) still has one.
	displayOnce     sync.Once
	displayMemoized *displayMemo

	// staged remembers which targets already hold the current spawner-job wrapper,
	// so StageJobScript is a no-op after the first success per target per server
	// run. Keyed by target identity (see stageKey); see StageJobScript.
	stagedMu sync.Mutex
	staged   map[string]bool
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
func (d *Driver) HostBin(bin string) { d.SetExec(TargetHost, HostExecutor{Bin: bin}) }

// SetExec registers (or replaces) a target's executor on a live driver — the one
// supported way to change Execs after construction, so the write is ordered
// against every concurrent lookup.
func (d *Driver) SetExec(t Target, e Executor) {
	d.execsMu.Lock()
	defer d.execsMu.Unlock()
	if d.Execs == nil {
		d.Execs = map[Target]Executor{}
	}
	d.Execs[t] = e
}

// sandboxExec is the guarded read of the sandbox executor, nil when unregistered
// (so the type assertions on it read as "no lifecycle/reaper support").
func (d *Driver) sandboxExec() Executor {
	e, _ := d.exec(TargetSandbox)
	return e
}

// exec is the guarded read of Execs. Every internal lookup goes through it.
func (d *Driver) exec(t Target) (Executor, bool) {
	d.execsMu.RLock()
	defer d.execsMu.RUnlock()
	e, ok := d.Execs[t]
	return e, ok
}

// SandboxEnabled reports whether the sandbox target is available (an executor is
// registered for it), so the spawn flow only offers "host or sandbox?" when
// sandbox sessions can actually run.
func (d *Driver) SandboxEnabled() bool {
	_, ok := d.exec(TargetSandbox)
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
func (d *Driver) AgentFor(s *Session) *agent.Agent { return d.agents().Resolve(s.Snapshot().Agent) }

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
		// Locked: this runs on the turn/reconcile path while another connection may be
		// renaming the record (Store.Rename writes Name in place).
		s.Read(func(s *Session) {
			name = s.Profile
			ctx.Session = s.Name
			ctx.Dir = s.Dir
		})
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
		if e, ok := d.exec(t); ok {
			return e
		}
	}
	e, _ := d.exec(TargetHost)
	return e
}

// EnsureContainer creates the session's persistent sandbox container if it isn't
// already running (called at spawn). A no-op for host sessions, or when the
// sandbox executor isn't registered / has no lifecycle support.
func (d *Driver) EnsureContainer(ctx context.Context, rec *Session) error {
	s := rec.Snapshot() // pure reader: one locked view, no live-record field reads
	if s.Target != TargetSandbox || s.Container == "" {
		return nil
	}
	if lc, ok := d.sandboxExec().(SandboxLifecycle); ok {
		p, err := d.ProfileFor(s)
		if err != nil {
			return err
		}
		return lc.Ensure(ctx, s, p)
	}
	return nil
}

// RemoveContainer destroys the session's persistent sandbox container (called on
// delete). A no-op for host sessions or when there's no sandbox lifecycle.
func (d *Driver) RemoveContainer(ctx context.Context, rec *Session) error {
	s := rec.Snapshot() // pure reader: one locked view
	if s.Target != TargetSandbox || s.Container == "" {
		return nil
	}
	if lc, ok := d.sandboxExec().(SandboxLifecycle); ok {
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
	reaper, ok := d.sandboxExec().(SandboxReaper)
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
	hostExec, _ := d.exec(TargetHost)
	if ex, ok := hostExec.(SSHExecutor); ok {
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
	// chainSig is a cheap freshness signature for a chain: two calls returning the
	// same non-empty string mean readTranscriptChain would return the same
	// messages. It's what lets a digest be cached across restarts without
	// re-parsing (see DigestCache). ok=false means this backend can't describe the
	// chain without doing the expensive read anyway — the caller then recomputes.
	chainSig(ids []string) (sig string, ok bool)
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
		// agy ignores our --conversation id and keys its store by internal brain-dir
		// ids we capture per turn (Session.AgyBrainIDs); antigravityFS replays those
		// brain transcripts. It records no token usage, so context stays absent.
		return antigravityFS{d.claudeFSFor(host)}
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
func (d *Driver) ArchiveSegment(live *Session) HistorySegment {
	rec := live.Snapshot() // the agent, host and id chain must describe ONE moment
	return HistorySegment{Agent: rec.Agent, Host: rec.Host, IDs: d.currentHistoryIDs(rec)}
}

// Digests is the durable cache backing DisplayDigest. Nil disables caching (the
// digest is then recomputed from a full parse every time, as before).
func (d *Driver) SetDigests(c *DigestCache) { d.digests = c }

// SetUsageCache installs the durable last-context-usage cache backing
// SessionContextUsage. Nil disables it (every lookup re-reads the transcript
// tail, as before).
func (d *Driver) SetUsageCache(c *UsageCache) { d.usage = c }

// display returns the driver's display-history memo, building it on first use.
func (d *Driver) display() *displayMemo {
	d.displayOnce.Do(func() { d.displayMemoized = newDisplayMemo() })
	return d.displayMemoized
}

// DisplayDigest returns a session's display-history digest (message count +
// content hash), reusing the cached value when every transcript in the chain is
// byte-for-byte where it was last time.
//
// This is the whole point of the digest cache: the app asks for every session's
// digest on connect to validate its offline transcript cache, and computing one
// otherwise means parsing that session's entire transcript chain. Statting the
// chain instead turns a multi-minute connect-time sweep into a handful of stats
// whenever nothing has changed — including right after a restart, when the
// in-memory parse memoization is empty and the old code was at its slowest.
//
// A backend that can't describe its chain cheaply (chainSig ok=false) falls back
// to the full read, so correctness never depends on the cache being available.
func (d *Driver) DisplayDigest(live *Session) (count int, hash string, cached bool, err error) {
	rec := live.Snapshot() // pure reader: one locked view for sig + read
	// Signature FIRST, then read. If a turn writes to the transcript in between,
	// we store a newer digest under an older signature — the next call sees a
	// changed signature, misses, and recomputes. Reading first and statting after
	// would fail the other way: a newer signature pinned to an older digest, which
	// never invalidates and leaves the app showing a stale transcript forever.
	parts := d.displayChainParts(rec)
	sig := parts.sig()
	if parts.ok {
		if count, hash, ok := d.digests.Get(rec.SessionID, sig); ok {
			return count, hash, true, nil
		}
	}
	msgs, err := d.readDisplayHistory(rec, parts)
	if err != nil {
		return 0, "", false, err
	}
	count, hash = HistoryDigest(msgs)
	if parts.ok {
		d.digests.Put(rec.SessionID, sig, count, hash)
	}
	return count, hash, false, nil
}

// DisplayHistory returns a session's full display history together with its
// digest, computing the chain's freshness signature ONCE for both.
//
// It's what the history op should call on a digest miss: DisplayDigest followed by
// ReadDisplayHistory would stat the whole chain twice (a round trip per transcript
// over SSH) and then re-derive a digest the cache may already hold.
func (d *Driver) DisplayHistory(live *Session) (msgs []Message, count int, hash string, err error) {
	rec := live.Snapshot()
	parts := d.displayChainParts(rec)
	sig := parts.sig()
	msgs, err = d.readDisplayHistory(rec, parts)
	if err != nil {
		return nil, 0, "", err
	}
	if parts.ok {
		if count, hash, ok := d.digests.Get(rec.SessionID, sig); ok {
			return msgs, count, hash, nil
		}
	}
	count, hash = HistoryDigest(msgs)
	if parts.ok {
		d.digests.Put(rec.SessionID, sig, count, hash)
	}
	return msgs, count, hash, nil
}

// chainParts is the freshness signature of everything ReadDisplayHistory reads,
// kept SPLIT per archived segment rather than pre-joined. The join answers "has
// anything changed?" (the digest cache); the parts answer "has THIS segment
// changed?", which is what lets an archived segment stay memoized across the turns
// that keep invalidating the session as a whole.
//
// ok is true only if EVERY part can describe itself — one opencode segment in a
// session's past makes the whole digest uncacheable, which is correct: we can't
// tell whether that segment changed.
type chainParts struct {
	segs []string // per archived segment, "<agent>@<sig>"; "" = indescribable
	cur  string   // the current chain, "<agent>@<sig>"; "" = indescribable
	ok   bool
}

// sig joins the parts into the single signature the digest cache keys on. Empty
// when any part is indescribable, which every cache treats as "never a hit".
func (p chainParts) sig() string {
	if !p.ok {
		return ""
	}
	var b strings.Builder
	for _, s := range p.segs {
		b.WriteString(s)
		b.WriteByte('|')
	}
	b.WriteString(p.cur)
	return b.String()
}

// displayChainParts stats every transcript ReadDisplayHistory would read — each
// archived segment plus the current chain, in that order — once.
func (d *Driver) displayChainParts(rec *Session) chainParts {
	p := chainParts{segs: make([]string, len(rec.History)), ok: true}
	for i, seg := range rec.History {
		if sig, ok := d.transcriptReaderFor(seg.Agent, seg.Host).chainSig(seg.IDs); ok {
			p.segs[i] = seg.Agent + "@" + sig
		} else {
			p.ok = false
		}
	}
	if sig, ok := d.transcriptReaderFor(rec.Agent, rec.Host).chainSig(d.currentHistoryIDs(rec)); ok {
		p.cur = rec.Agent + "@" + sig
	} else {
		p.ok = false
	}
	return p
}

// ReadDisplayHistory reads a session's full cross-backend chat log for display: each
// archived HistorySegment (a previous backend) via that backend's own reader, oldest
// first, then the current backend's chain — concatenated and re-indexed contiguously
// so pagination cursors stay stable across the whole log. A failed archived segment is
// logged and skipped (best-effort scrollback); only the current backend's read fails
// the call, matching pre-split behavior. With no History this equals the old
// ReadTranscriptChain(current) exactly.
func (d *Driver) ReadDisplayHistory(live *Session) ([]Message, error) {
	rec := live.Snapshot() // pure reader: History/Agent/ids from one locked view
	return d.readDisplayHistory(rec, d.displayChainParts(rec))
}

// readDisplayHistory is ReadDisplayHistory against an already-taken snapshot and
// an already-computed chain signature, served from the display memo when nothing
// it reads has moved (and per archived segment when only the current chain has).
func (d *Driver) readDisplayHistory(rec *Session, parts chainParts) ([]Message, error) {
	if msgs, ok := d.display().getWhole(rec.SessionID, parts.sig()); ok {
		return msgs, nil
	}
	var all []Message
	degraded := false // a segment read failed: this log is short, don't memoize it
	for i, seg := range rec.History {
		key := ""
		if i < len(parts.segs) {
			key = parts.segs[i]
		}
		if msgs, ok := d.display().getSegment(key); ok {
			all = append(all, msgs...)
			continue
		}
		msgs, err := d.transcriptReaderFor(seg.Agent, seg.Host).readTranscriptChain(seg.IDs)
		if err != nil {
			log.Printf("display history[%s]: read archived %s segment: %v", rec.Name, seg.Agent, err)
			degraded = true
			continue
		}
		d.display().putSegment(key, msgs)
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
	if !degraded {
		d.display().putWhole(rec.SessionID, parts.sig(), all)
	}
	return all, nil
}

// DeleteSessionAll purges every on-disk transcript of a session across ALL the
// backends it ran: each archived History segment via its own backend reader, plus
// the current backend's chain. Use for a full session delete so a backend switched
// away from doesn't orphan its transcripts. Returns the count removed; best-effort
// per segment (a segment error is logged, not fatal).
func (d *Driver) DeleteSessionAll(live *Session) (int, error) {
	rec := live.Snapshot() // pure reader: one locked view of the whole chain
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

// SessionContextUsage is LastContextUsage for a registered session, served from
// the durable UsageCache whenever the session's transcripts are byte-for-byte
// where they were when the snapshot was last read.
//
// Prefer it over LastContextUsage anywhere a *Session is in hand — above all on
// attach, which blocks the `attached` ack on this lookup. See UsageCache for why
// the underlying read is expensive and why the in-memory cache can't cover it.
func (d *Driver) SessionContextUsage(live *Session) *ContextSnapshot {
	rec := live.Snapshot() // one locked view: the sig and the ids must describe the same moment
	ids := rec.TranscriptIDs()
	reader := d.transcriptReaderFor(rec.Agent, rec.Host)
	// Signature FIRST, then read — same ordering rule as DisplayDigest: a turn
	// landing in between stores a newer snapshot under an older signature, which
	// simply misses next time. The reverse pins a newer signature to an older
	// snapshot, which never invalidates.
	sig, cacheable := reader.chainSig(ids)
	if cacheable {
		sig = rec.Agent + "@" + sig
		if snap, ok := d.usage.Get(rec.SessionID, sig); ok {
			return snap
		}
	}
	snap := reader.lastContextUsage(ids)
	if cacheable {
		d.usage.Put(rec.SessionID, sig, snap)
	}
	return snap
}

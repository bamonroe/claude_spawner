// Package session drives Claude Code in headless stream-json mode and tracks
// sessions as durable records (a session_id on disk tied to a directory), not
// live processes. This is the data path for the voice interface: each dictated
// turn shells out to `claude -p --output-format stream-json` and the clean
// `result` text is returned for text-to-speech. See docs/protocol.md and the
// "TUI capture" decision in CLAUDE.md.
package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// newLineScanner returns a bufio.Scanner for newline-delimited JSON. It starts
// with a modest 64 KB buffer but allows lines to grow to 16 MB, since a single
// tool-use event's input can far exceed bufio's default 64 KB line cap. Shared
// by the stream-json parser and the transcript/discover readers.
func newLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	return sc
}

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

// Driver runs Claude Code turns. It holds no per-session state.
type Driver struct {
	// Execs maps an execution Target to the Executor that launches its turns. Turn
	// and Usage select by the session's Target (empty/unknown falls back to
	// TargetHost, which must always be registered). Register a sandbox target to
	// make host-vs-container a per-session choice.
	Execs map[Target]Executor
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
		return lc.Ensure(ctx, s.Container, s.Dir)
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
// app's "restart" button). The command is run detached in its own process group
// via `sh -c`, so it survives the server's own termination when it restarts the
// unit (the systemd unit must use KillMode=process). It returns once the rebuild
// is LAUNCHED — the process is replaced moments later — or an error if restart
// isn't configured. Errors from the detached command are logged, not returned.
func (d *Driver) Restart(ctx context.Context) error {
	if d.RestartCmd == "" {
		return fmt.Errorf("server restart is not configured (set SPAWNER_RESTART_CMD)")
	}
	cmd := exec.Command("sh", "-c", d.RestartCmd)
	// Own process group so a `systemctl restart` inside the command doesn't take
	// this detached child down with the server (KillMode=process on the unit does
	// the rest — only the main process is killed, not the whole cgroup).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("restart: launched %q", d.RestartCmd)
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("restart command failed: %v", err)
		}
	}()
	return nil
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

// DeleteSessionByIDs removes exactly the given session_ids' transcripts (one
// logical session) on the session's host (empty host = local), leaving its
// dir-mates intact.
func (d *Driver) DeleteSessionByIDs(ctx context.Context, host string, ids []string) (int, error) {
	return d.claudeFSFor(host).deleteByIDs(ids)
}

// ReadTranscriptChain reads a session's full history (current + rotated prior ids)
// from its host (empty host = local), re-indexed contiguously for pagination.
func (d *Driver) ReadTranscriptChain(host string, ids []string) ([]Message, error) {
	return d.claudeFSFor(host).readTranscriptChain(ids)
}

// LastContextUsage returns a session's live context snapshot (last usage-bearing
// assistant turn) read from its host (empty host = local); nil if none yet.
func (d *Driver) LastContextUsage(host string, ids []string) *ContextSnapshot {
	return d.claudeFSFor(host).lastContextUsage(ids)
}

// ToolUse describes a tool Claude invoked during a turn. FilePath is set for
// file-editing tools (Edit/Write/MultiEdit/NotebookEdit) so the caller can show
// which file changed.
type ToolUse struct {
	Name     string
	FilePath string
}

// Usage is the token accounting for one turn, read from the stream-json `result`
// event's aggregate `usage`. CacheRead > 0 means the turn reused a warm prompt
// cache (cheap/fast); CacheWrite > 0 means it (re)built the cache — the signal
// behind the app's cache-warm indicator. The zero value means the turn reported
// no usage. The json tags are the on-wire names sent to the app (see the
// `output` message in docs/protocol.md).
type Usage struct {
	Input      int `json:"input"`       // input_tokens
	Output     int `json:"output"`      // output_tokens
	CacheWrite int `json:"cache_write"` // cache_creation_input_tokens (cache (re)built)
	CacheRead  int `json:"cache_read"`  // cache_read_input_tokens (warm-cache hit)
}

// RateLimit is the subscription usage-window state carried by the stream-json
// `rate_limit_event` (emitted early in every turn). It is how the app shows the
// Claude plan's session limit. Status is a COARSE signal — "allowed" until you
// near/hit the cap — since Anthropic does not expose an exact remaining quota.
// ResetsAt is unix seconds; Type names the binding window ("five_hour" | weekly).
// The zero value (empty Type) means the turn carried no rate-limit event.
type RateLimit struct {
	Status       string `json:"status"`        // "allowed" | warning/rejected as the cap nears
	ResetsAt     int64  `json:"resets_at"`     // unix seconds when this window resets
	Type         string `json:"limit_type"`    // "five_hour" (rolling session) | weekly
	UsingOverage bool   `json:"using_overage"` // currently drawing on pay-as-you-go overage
}

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
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose"}
	if s.Started {
		args = append(args, "--resume", s.SessionID)
	} else {
		args = append(args, "--session-id", s.SessionID)
	}
	if d.Bypass {
		args = append(args, "--dangerously-skip-permissions")
	}

	// Launch via the session's execution target (host by default). The executor
	// owns process-group/abort semantics; Turn only builds args and parses stdout.
	proc, err := d.executor(s.Target).Start(ctx, s, args)
	if err != nil {
		return "", Usage{}, err
	}

	// Once claude has launched with --session-id it has created (or is creating)
	// the session on disk. Flip Started here — NOT after a clean Wait — so that a
	// turn interrupted mid-stream (client drop, container restart) still records
	// that the id now exists. Otherwise the next turn re-runs --session-id on an
	// id claude already owns, which exits status 1 forever, bricking the session.
	// The caller persists this even on the error path (see gateway/jobs.go).
	s.Started = true

	reply, usage, perr := parseStream(proc.Stdout(), onTool, onText, onRateLimit)
	if werr := proc.Wait(); werr != nil {
		return "", Usage{}, fmt.Errorf("claude exited: %w", werr)
	}
	if perr != nil {
		return "", Usage{}, perr
	}
	return reply, usage, nil
}

// streamEvent is the subset of the stream-json schema we consume. Unknown fields
// are ignored; non-JSON lines are skipped by the scanner loop. Tool use is read
// from the full `assistant` message content (always present), not stream_event
// deltas (which require --include-partial-messages).
type streamEvent struct {
	Type    string `json:"type"`    // "system" | "assistant" | "user" | "result" | ...
	Subtype string `json:"subtype"` // on result: "success" | "error_*"
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	// Usage is the turn's aggregate token accounting, present on the `result`
	// event. Field names are Anthropic's; we remap into our own Usage type.
	Usage struct {
		InputTokens         int `json:"input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		CacheCreationTokens int `json:"cache_creation_input_tokens"`
		CacheReadTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	// RateLimitInfo is present on the `rate_limit_event`. Anthropic's field names;
	// remapped into our RateLimit type.
	RateLimitInfo struct {
		Status         string `json:"status"`
		ResetsAt       int64  `json:"resetsAt"`
		RateLimitType  string `json:"rateLimitType"`
		IsUsingOverage bool   `json:"isUsingOverage"`
	} `json:"rate_limit_info"`
	Message struct {
		Content []struct {
			Type  string `json:"type"` // "text" | "tool_use"
			Text  string `json:"text"` // prose when Type=="text"
			Name  string `json:"name"` // tool name when Type=="tool_use"
			Input struct {
				FilePath     string `json:"file_path"`
				NotebookPath string `json:"notebook_path"`
			} `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

// parseStream reads NDJSON until EOF, returning the final result text. It calls
// onTool per tool_use block and onText per assistant text message (in stream
// order) so the caller can render tool breadcrumbs and Claude's prose live.
func parseStream(r interface{ Read([]byte) (int, error) }, onTool func(ToolUse), onText func(string), onRateLimit func(RateLimit)) (string, Usage, error) {
	sc := newLineScanner(r)

	var result string
	var usage Usage
	var gotResult, isError bool
	var subtype string
	var malformed int // non-blank lines that weren't parseable JSON events
	for sc.Scan() {
		line := sc.Bytes()
		var ev streamEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if len(strings.TrimSpace(string(line))) > 0 {
				malformed++ // count corruption, but keep scanning for a result
			}
			continue // defensively skip anything that isn't a JSON event
		}
		switch ev.Type {
		case "assistant":
			// One assistant event carries a whole message (text and/or tool_use
			// blocks). Fan tool breadcrumbs out via onTool and the joined prose via
			// onText, in the order the blocks appear.
			var text strings.Builder
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "tool_use":
					if onTool != nil {
						path := b.Input.FilePath
						if path == "" {
							path = b.Input.NotebookPath
						}
						onTool(ToolUse{Name: b.Name, FilePath: path})
					}
				case "text":
					if b.Text != "" {
						if text.Len() > 0 {
							text.WriteByte('\n')
						}
						text.WriteString(b.Text)
					}
				}
			}
			if onText != nil && text.Len() > 0 {
				onText(text.String())
			}
		case "result":
			result, gotResult = ev.Result, true
			usage = Usage{
				Input:      ev.Usage.InputTokens,
				Output:     ev.Usage.OutputTokens,
				CacheWrite: ev.Usage.CacheCreationTokens,
				CacheRead:  ev.Usage.CacheReadTokens,
			}
			isError = ev.IsError || (ev.Subtype != "" && ev.Subtype != "success")
			subtype = ev.Subtype
		case "rate_limit_event":
			if onRateLimit != nil && ev.RateLimitInfo.RateLimitType != "" {
				onRateLimit(RateLimit{
					Status:       ev.RateLimitInfo.Status,
					ResetsAt:     ev.RateLimitInfo.ResetsAt,
					Type:         ev.RateLimitInfo.RateLimitType,
					UsingOverage: ev.RateLimitInfo.IsUsingOverage,
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", Usage{}, fmt.Errorf("read stream: %w", err)
	}
	if !gotResult {
		if malformed > 0 {
			return "", Usage{}, fmt.Errorf("stream corrupted: ended without a result event (%d malformed lines)", malformed)
		}
		return "", Usage{}, fmt.Errorf("stream ended without a result event")
	}
	if isError {
		return "", Usage{}, fmt.Errorf("claude turn failed (%s): %s", subtype, result)
	}
	return result, usage, nil
}

// Usage runs `claude -p "/usage"` headless and returns its report text (the
// stream-json `result`) — the same session/weekly percent-used breakdown the TUI
// `/usage` command shows. It is account-global (no session_id/dir), so it runs in
// a temp dir. This is a real, if lightweight, claude invocation, so callers should
// treat it as on-demand rather than per-turn.
func (d *Driver) Usage(ctx context.Context) (string, error) {
	args := []string{"-p", "/usage", "--output-format", "stream-json", "--verbose"}
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
	// Account-global probe: run it on the loopback host explicitly (the SSH executor
	// no longer defaults a hostless session). A purely remote deployment with no
	// reachable local box can't run /usage; that's an accepted limitation.
	proc, err := d.executor(TargetHost).Start(ctx, &Session{Name: "usage", Dir: dir, Host: LocalHost}, args)
	if err != nil {
		return "", err
	}
	reply, _, perr := parseStream(proc.Stdout(), nil, nil, nil)
	if werr := proc.Wait(); werr != nil {
		return "", fmt.Errorf("claude exited: %w", werr)
	}
	if perr != nil {
		return "", perr
	}
	return reply, nil
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

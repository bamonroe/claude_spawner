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
	// Bin is the claude binary (default "claude").
	Bin string
	// Bypass adds --dangerously-skip-permissions when true (project default).
	Bypass bool
}

// NewDriver returns a Driver with project defaults.
func NewDriver() *Driver { return &Driver{Bin: "claude", Bypass: true} }

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

	cmd := exec.CommandContext(ctx, d.Bin, args...)
	cmd.Dir = s.Dir
	// Run claude in its own process group and, on ctx cancel (an abort), kill the
	// whole group — so claude AND any tool child it spawned (a build, a sleep) die,
	// not just the top-level process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", Usage{}, err
	}
	if err := cmd.Start(); err != nil {
		return "", Usage{}, fmt.Errorf("start claude: %w", err)
	}

	reply, usage, perr := parseStream(stdout, onTool, onText, onRateLimit)
	if werr := cmd.Wait(); werr != nil {
		return "", Usage{}, fmt.Errorf("claude exited: %w", werr)
	}
	if perr != nil {
		return "", Usage{}, perr
	}
	s.Started = true
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
	for sc.Scan() {
		var ev streamEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
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
		return "", Usage{}, fmt.Errorf("stream ended without a result event")
	}
	if isError {
		return "", Usage{}, fmt.Errorf("claude turn failed (%s): %s", subtype, result)
	}
	return result, usage, nil
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

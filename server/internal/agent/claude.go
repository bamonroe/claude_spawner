package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The Claude Code backend, self-contained: registry entry, per-turn command
// line, and the stream-json output parser. This file is the template for adding
// a backend — see "Adding an AI backend" in docs/architecture.md.

// claude builds the Claude Code backend entry. Its Args reproduce the command
// line session.Driver.Turn used before the registry existed, plus the model flag.
func claude() *Agent {
	return &Agent{
		ID:           "claude",
		Name:         "Claude Code",
		Transcript:   TranscriptClaude,
		DefaultModel: "opus",
		Models: []Model{
			// Behaviour-preserving default: opus matches how the CLI was already
			// being driven here. Aliases opus/sonnet are what `claude --model`
			// accepts directly; fable uses the full id to avoid alias ambiguity.
			{Alias: "opus", Flag: "opus"},
			{Alias: "sonnet", Flag: "sonnet"},
			{Alias: "fable", Flag: "claude-fable-5", Spoken: []string{"fable five", "fable5"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			args := []string{"-p", s.Prompt, "--output-format", "stream-json", "--verbose"}
			if s.Resume {
				args = append(args, "--resume", s.SessionID)
			} else {
				args = append(args, "--session-id", s.SessionID)
			}
			if s.Bypass {
				args = append(args, "--dangerously-skip-permissions")
			}
			// Injected settings carry the PreToolUse hook that blocks background bash
			// (which can't survive a turn) and redirects Claude to spawner-job. Hooks
			// fire even under --dangerously-skip-permissions, so this is real
			// enforcement, not just the priming instruction.
			if s.SettingsJSON != "" {
				args = append(args, "--settings", s.SettingsJSON)
			}
			if len(m.Args) > 0 {
				args = append(args, m.Args...)
			} else if m.Flag != "" {
				args = append(args, "--model", m.Flag)
			}
			return args
		},
		ParseTurn: parseClaudeStream,
	}
}

// claudeEvent is the subset of the `--output-format stream-json` NDJSON schema
// we consume. Unknown fields are ignored; non-JSON lines are skipped by the
// scanner loop. Tool use is read from the full `assistant` message content
// (always present), not stream_event deltas (which require
// --include-partial-messages).
type claudeEvent struct {
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

// parseClaudeStream reads stream-json NDJSON until EOF, returning the final
// `result` text and usage. It calls cb.OnTool per tool_use block and cb.OnText
// per assistant text message (in stream order) so the caller can render tool
// breadcrumbs and Claude's prose live. Claude accepts a caller-supplied session
// id, so TurnResult.SessionID is always empty here.
func parseClaudeStream(r io.Reader, cb TurnCallbacks) (TurnResult, error) {
	sc := NewLineScanner(r)

	var res TurnResult
	var gotResult, isError bool
	var subtype string
	var malformed int // non-blank lines that weren't parseable JSON events
	for sc.Scan() {
		line := sc.Bytes()
		var ev claudeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if len(strings.TrimSpace(string(line))) > 0 {
				malformed++ // count corruption, but keep scanning for a result
			}
			continue // defensively skip anything that isn't a JSON event
		}
		switch ev.Type {
		case "assistant":
			// One assistant event carries a whole message (text and/or tool_use
			// blocks). Fan tool breadcrumbs out via OnTool and the joined prose via
			// OnText, in the order the blocks appear.
			var text strings.Builder
			for _, b := range ev.Message.Content {
				switch b.Type {
				case "tool_use":
					if cb.OnTool != nil {
						path := b.Input.FilePath
						if path == "" {
							path = b.Input.NotebookPath
						}
						cb.OnTool(ToolUse{Name: b.Name, FilePath: path})
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
			if cb.OnText != nil && text.Len() > 0 {
				cb.OnText(text.String())
			}
		case "result":
			res.Reply, gotResult = ev.Result, true
			res.Usage = Usage{
				Input:      ev.Usage.InputTokens,
				Output:     ev.Usage.OutputTokens,
				CacheWrite: ev.Usage.CacheCreationTokens,
				CacheRead:  ev.Usage.CacheReadTokens,
			}
			isError = ev.IsError || (ev.Subtype != "" && ev.Subtype != "success")
			subtype = ev.Subtype
		case "rate_limit_event":
			if cb.OnRateLimit != nil && ev.RateLimitInfo.RateLimitType != "" {
				cb.OnRateLimit(RateLimit{
					Status:       ev.RateLimitInfo.Status,
					ResetsAt:     ev.RateLimitInfo.ResetsAt,
					Type:         ev.RateLimitInfo.RateLimitType,
					UsingOverage: ev.RateLimitInfo.IsUsingOverage,
				})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return TurnResult{}, fmt.Errorf("read stream: %w", err)
	}
	if !gotResult {
		if malformed > 0 {
			return TurnResult{}, fmt.Errorf("stream corrupted: ended without a result event (%d malformed lines)", malformed)
		}
		return TurnResult{}, fmt.Errorf("stream ended without a result event")
	}
	if isError {
		return TurnResult{}, fmt.Errorf("claude turn failed (%s): %s", subtype, res.Reply)
	}
	return res, nil
}

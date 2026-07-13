package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The Codex CLI backend, self-contained: registry entry, per-turn command line,
// and the `codex exec --json` JSONL parser.

// codex builds the Codex CLI backend entry. Codex runs non-interactively via
// `codex exec`; unlike Claude it mints its OWN session id (the thread_id, read
// from the first output event) rather than accepting a caller-supplied one, so
// the first turn omits any id and the session driver captures thread_id from the
// stream. Resume replays via `codex exec resume <id>`. The working directory is
// set by the Executor (the process cwd), so no -C is needed. On this account the
// supported model is gpt-5.5; the alternates are reasoning-effort presets on it
// (plan-independent), which is why ordinal selection ("use model 2") matters —
// the labels are awkward to say.
func codex() *Agent {
	return &Agent{
		ID:            "codex",
		Name:          "Codex CLI",
		Bin:           "codex",
		Transcript:    TranscriptCodex,
		SelfAssignsID: true,
		DefaultModel:  "gpt-5.5",
		Models: []Model{
			{Alias: "gpt-5.5", Flag: "gpt-5.5", Spoken: []string{"five five", "gpt five five", "standard"}},
			{Alias: "gpt-5.5-high", Args: []string{"-m", "gpt-5.5", "-c", "model_reasoning_effort=high"}, Spoken: []string{"high", "high reasoning", "thorough"}},
			{Alias: "gpt-5.5-low", Args: []string{"-m", "gpt-5.5", "-c", "model_reasoning_effort=low"}, Spoken: []string{"low", "low reasoning", "fast"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			args := []string{"exec"}
			if s.Resume {
				args = append(args, "resume", s.SessionID)
			}
			// Options before the positional prompt; `--` terminates flags so a dictated
			// prompt starting with "-" (or the word "resume") can't be misparsed as one.
			args = append(args, "--json", "--skip-git-repo-check")
			if s.Bypass {
				args = append(args, "--dangerously-bypass-approvals-and-sandbox")
			}
			if len(m.Args) > 0 {
				args = append(args, m.Args...)
			} else if m.Flag != "" {
				args = append(args, "-m", m.Flag)
			}
			args = append(args, "--", s.Prompt)
			return args
		},
		ParseTurn: parseCodexStream,
	}
}

// codexEvent is the subset of Codex CLI's `codex exec --json` JSONL we consume.
// Unknown fields/events are ignored; non-JSON lines are skipped.
type codexEvent struct {
	Type     string `json:"type"`      // "thread.started" | "turn.started" | "item.completed" | "turn.completed" | "turn.failed" | "error"
	ThreadID string `json:"thread_id"` // on thread.started: the session id
	Item     struct {
		Type    string `json:"type"`    // "agent_message" | "error" | "command_execution" | "file_change" | ...
		Text    string `json:"text"`    // reply prose on agent_message
		Message string `json:"message"` // error text on an error item
	} `json:"item"`
	Usage struct {
		InputTokens         int `json:"input_tokens"`
		CachedInputTokens   int `json:"cached_input_tokens"`
		OutputTokens        int `json:"output_tokens"`
		ReasoningOutputToks int `json:"reasoning_output_tokens"`
	} `json:"usage"` // on turn.completed
	Message string `json:"message"` // on a top-level error event
	Error   struct {
		Message string `json:"message"`
	} `json:"error"` // on turn.failed
}

// parseCodexStream reads Codex's `--json` JSONL until EOF, returning the final
// agent_message text, token usage, and the session's thread_id (Codex's id, read
// from the first event) in TurnResult.SessionID. Tool/step items are fanned out
// via cb.OnTool and each agent_message via cb.OnText, mirroring the Claude
// parser. The thread_id is returned on every path (even errors) so a first turn
// that fails after thread.started is still resumable. A turn.failed / error
// event (or an error item) fails the turn.
func parseCodexStream(r io.Reader, cb TurnCallbacks) (TurnResult, error) {
	sc := NewLineScanner(r)

	var res TurnResult
	var failMsg string
	var gotReply bool
	var malformed int
	for sc.Scan() {
		line := sc.Bytes()
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if len(strings.TrimSpace(string(line))) > 0 {
				malformed++
			}
			continue
		}
		switch ev.Type {
		case "thread.started":
			if ev.ThreadID != "" {
				res.SessionID = ev.ThreadID
			}
		case "item.completed":
			switch ev.Item.Type {
			case "agent_message":
				res.Reply, gotReply = ev.Item.Text, true
				if cb.OnText != nil && ev.Item.Text != "" {
					cb.OnText(ev.Item.Text)
				}
			case "error":
				if failMsg == "" {
					failMsg = ev.Item.Message
				}
			default:
				// A step Codex took (command_execution, file_change, reasoning, …):
				// surface it as a tool breadcrumb, named by the item type.
				if cb.OnTool != nil && ev.Item.Type != "" {
					cb.OnTool(ToolUse{Name: ev.Item.Type})
				}
			}
		case "turn.completed":
			res.Usage = Usage{
				Input:      ev.Usage.InputTokens,
				Output:     ev.Usage.OutputTokens + ev.Usage.ReasoningOutputToks,
				CacheRead:  ev.Usage.CachedInputTokens,
				CacheWrite: 0, // Codex reports no separate cache-write count
			}
		case "turn.failed":
			if failMsg == "" {
				failMsg = ev.Error.Message
			}
		case "error":
			if failMsg == "" {
				failMsg = ev.Message
			}
		}
	}
	// Every error path still carries res.SessionID so a failed first turn
	// remains resumable (the caller adopts the id before checking the error).
	if err := sc.Err(); err != nil {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("read codex stream: %w", err)
	}
	if failMsg != "" {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("codex turn failed: %s", failMsg)
	}
	if !gotReply {
		if malformed > 0 {
			return TurnResult{SessionID: res.SessionID}, fmt.Errorf("codex stream corrupted: no agent message (%d malformed lines)", malformed)
		}
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("codex stream ended without an agent message")
	}
	return res, nil
}

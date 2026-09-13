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
// set by the Executor (the process cwd), so no -C is needed. Codex model
// availability is account- and rollout-dependent, so the backend discovers the
// host CLI's live catalogue with `codex debug models`; the compiled list below
// is only the fallback when that probe is unavailable.
func codex() *Agent {
	return &Agent{
		ID:            "codex",
		Name:          "Codex CLI",
		Bin:           "codex",
		Transcript:    TranscriptCodex,
		SelfAssignsID: true,
		DefaultModel:  "gpt-6-astra",
		Models: codexModels([]codexModelSpec{
			{Slug: "gpt-6-astra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
			{Slug: "gpt-5.6-sol", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
			{Slug: "gpt-5.6-terra", Efforts: []string{"low", "medium", "high", "xhigh", "max", "ultra"}},
			{Slug: "gpt-5.6-luna", Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
			{Slug: "gpt-5.5", Efforts: []string{"low", "medium", "high", "xhigh"}},
		}),
		DiscoverArgs: []string{"debug", "models"},
		ParseModels:  parseCodexModels,
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

type codexModelSpec struct {
	Slug    string
	Efforts []string
}

func codexModels(specs []codexModelSpec) []Model {
	var out []Model
	for _, spec := range specs {
		out = append(out, Model{
			Alias:  spec.Slug,
			Flag:   spec.Slug,
			Spoken: codexModelSpoken(spec.Slug),
		})
		for _, effort := range spec.Efforts {
			out = append(out, Model{
				Alias:  spec.Slug + "-" + effort,
				Args:   []string{"-m", spec.Slug, "-c", "model_reasoning_effort=" + effort},
				Spoken: codexEffortSpoken(spec.Slug, effort),
			})
		}
	}
	return out
}

func codexModelSpoken(slug string) []string {
	switch slug {
	case "gpt-6-astra":
		return []string{"astra", "six astra", "gpt six astra"}
	case "gpt-5.6-sol":
		return []string{"sol", "five six sol", "gpt five six sol"}
	case "gpt-5.6-terra":
		return []string{"terra", "five six terra", "gpt five six terra"}
	case "gpt-5.6-luna":
		return []string{"luna", "five six luna", "gpt five six luna"}
	case "gpt-5.5":
		return []string{"five five", "gpt five five"}
	default:
		return nil
	}
}

func codexEffortSpoken(slug, effort string) []string {
	base := strings.TrimPrefix(slug, "gpt-")
	base = strings.ReplaceAll(base, "-", " ")
	base = strings.ReplaceAll(base, ".", " ")
	label := strings.ReplaceAll(effort, "xhigh", "extra high")
	return []string{base + " " + label, label + " " + base}
}

type codexModelsJSON struct {
	Models []struct {
		Slug                     string `json:"slug"`
		Visibility               string `json:"visibility"`
		SupportedReasoningLevels []struct {
			Effort string `json:"effort"`
		} `json:"supported_reasoning_levels"`
	} `json:"models"`
}

func parseCodexModels(stdout []byte) []Model {
	var payload codexModelsJSON
	if err := json.Unmarshal(stdout, &payload); err != nil {
		return nil
	}
	specs := make([]codexModelSpec, 0, len(payload.Models))
	for _, raw := range payload.Models {
		if raw.Slug == "" || raw.Visibility != "list" {
			continue
		}
		var efforts []string
		for _, level := range raw.SupportedReasoningLevels {
			if level.Effort != "" {
				efforts = append(efforts, level.Effort)
			}
		}
		specs = append(specs, codexModelSpec{Slug: raw.Slug, Efforts: efforts})
	}
	return codexModels(specs)
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

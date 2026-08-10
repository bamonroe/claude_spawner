package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// opencode CLI-backed providers: registry entries, per-turn command line, and
// the `opencode run --format json` event parser. Ollama and Zen share the same
// opencode runner and transcript layout; they differ only in the provider/model
// prefix passed to opencode and the discovery probe.

// ollama builds the local Ollama provider exposed through opencode. The legacy
// "opencode" id remains an alias so older sessions and provider overrides still
// resolve here, while the UI advertises the clearer "Ollama" backend.
func ollama() *Agent {
	return opencodeProvider(opencodeProviderSpec{
		ID:           "ollama",
		Aliases:      []string{"opencode"},
		Name:         "Ollama",
		Provider:     "ollama",
		DefaultModel: "qwen2.5-coder:7b",
		Models: []Model{
			{Alias: "qwen2.5-coder:7b", Flag: "ollama/qwen2.5-coder:7b", Spoken: []string{"qwen", "coder", "qwen coder", "qwen two five", "qwen 2.5"}},
			{Alias: "llama3.1:8b", Flag: "ollama/llama3.1:8b", Spoken: []string{"llama", "llama three", "llama 3", "llama 3.1"}},
		},
	})
}

// zen builds the OpenCode Zen subscription provider exposed through opencode.
// OpenCode documents Zen model ids with the `opencode/<model-id>` prefix, so the
// backend probes and runs provider "opencode" while advertising itself as Zen.
func zen() *Agent {
	return opencodeProvider(opencodeProviderSpec{
		ID:           "zen",
		Name:         "Zen",
		Provider:     "opencode",
		DefaultModel: "gpt-5.5",
		Models: []Model{
			{Alias: "gpt-5.5", Flag: "opencode/gpt-5.5", Spoken: []string{"gpt five five", "five five"}},
			{Alias: "claude-sonnet-4-5", Flag: "opencode/claude-sonnet-4-5", Spoken: []string{"sonnet", "sonnet four five", "claude sonnet"}},
			{Alias: "gemini-3.1-pro", Flag: "opencode/gemini-3.1-pro", Spoken: []string{"gemini pro", "pro"}},
			{Alias: "qwen3.7-plus", Flag: "opencode/qwen3.7-plus", Spoken: []string{"qwen plus"}},
		},
	})
}

type opencodeProviderSpec struct {
	ID           string
	Aliases      []string
	Name         string
	Provider     string
	DefaultModel string
	Models       []Model
}

// opencodeProvider builds one opencode-backed entry. opencode runs
// non-interactively via `opencode run`; like Codex it mints its OWN session id
// (a `ses_...` id announced as `sessionID` on every stream event) rather than
// accepting a caller-supplied one, so the first turn omits any id and the session
// driver adopts it from the stream. Resume replays via `opencode run -s <id>`.
// The working directory is the process cwd (set by the Executor), so no --dir is
// needed. Models use opencode's `provider/model` form.
//
// Transcript: opencode persists sessions in a SQLite database, so it declares
// TranscriptOpencode — its reader (session/opencode_transcript.go) shells out to
// opencode's own `export`/`session delete` commands to replay history and read
// context usage on reattach.
func opencodeProvider(spec opencodeProviderSpec) *Agent {
	return &Agent{
		ID:            spec.ID,
		Aliases:       spec.Aliases,
		Name:          spec.Name,
		Bin:           "opencode",
		Transcript:    TranscriptOpencode,
		SelfAssignsID: true,
		DefaultModel:  spec.DefaultModel,
		Models:        spec.Models,
		// Live discovery replaces the compiled fallback whenever opencode reports
		// the provider's configured models, so catalogue changes appear in the app
		// without a server rebuild.
		DiscoverArgs: []string{"models", spec.Provider},
		ParseModels:  parseOpencodeModels,
		build: func(a *Agent, s TurnSpec, m Model) []string {
			args := []string{"run"}
			if s.Resume {
				args = append(args, "-s", s.SessionID)
			}
			args = append(args, "--format", "json")
			if s.Bypass {
				// Auto-approve permissions (opencode's skip-permissions equivalent).
				args = append(args, "--auto")
			}
			if len(m.Args) > 0 {
				args = append(args, m.Args...)
			} else if m.Flag != "" {
				args = append(args, "-m", m.Flag)
			}
			// `--` terminates flags so a dictated prompt starting with "-" isn't
			// misparsed as one; opencode passes the remainder as the message.
			args = append(args, "--", s.Prompt)
			return args
		},
		ParseTurn: parseOpencodeStream,
	}
}

// opencodeEvent is the subset of opencode's `run --format json` JSONL we consume.
// Every line is an envelope carrying a message `part`; the part's `type`
// discriminates text / tool / step boundaries. Unknown fields/events are
// ignored, non-JSON lines are skipped. The `sessionID` on every event is
// opencode's own id (adopted as the session id on the first turn).
type opencodeEvent struct {
	Type      string `json:"type"`      // envelope kind, mirrors the part type ("text","tool","step_start","step_finish","error")
	SessionID string `json:"sessionID"` // opencode's session id, present on every event
	Part      struct {
		Type      string `json:"type"`      // "text" | "tool" | "step-start" | "step-finish"
		Text      string `json:"text"`      // reply prose on a text part
		Synthetic bool   `json:"synthetic"` // injected (non-model) text — skipped
		Ignored   bool   `json:"ignored"`   // text opencode marks not-for-display — skipped
		Tool      string `json:"tool"`      // tool name on a tool part
		State     struct {
			Status string         `json:"status"` // "running" | "completed" | "error"
			Error  string         `json:"error"`  // tool error text on status=="error"
			Input  map[string]any `json:"input"`  // tool input; filePath present for file tools
		} `json:"state"`
		Tokens struct {
			Input     int `json:"input"`
			Output    int `json:"output"`
			Reasoning int `json:"reasoning"`
			Cache     struct {
				Read  int `json:"read"`
				Write int `json:"write"`
			} `json:"cache"`
		} `json:"tokens"` // on a step-finish part
	} `json:"part"`
	Error struct {
		Message string `json:"message"`
		Data    struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"` // on a top-level error event (defensive; most errors exit non-zero on stderr)
}

// parseOpencodeStream reads opencode's `--format json` JSONL until EOF, returning
// the assembled reply text, the final step's token usage, and opencode's session
// id (read from the stream) in TurnResult.SessionID, and TurnResult.Turns counted
// from the stream's step-finish parts (one per model cycle). Text parts are concatenated
// (synthetic/ignored ones skipped) and fanned out live via cb.OnText; tool parts
// become cb.OnTool breadcrumbs. A tool that itself errors is only a breadcrumb —
// opencode may recover — so it doesn't fail the turn; a top-level error event
// does. The session id is returned on every path (it precedes any failure) so a
// first turn that fails after the id lands is still resumable. Process-level
// failures (bad resume id, provider unreachable) exit non-zero and surface via
// the driver's proc.Wait, not here.
func parseOpencodeStream(r io.Reader, cb TurnCallbacks) (TurnResult, error) {
	sc := NewLineScanner(r)

	var res TurnResult
	var reply strings.Builder
	var gotReply bool
	var failMsg string
	var malformed int
	for sc.Scan() {
		line := sc.Bytes()
		var ev opencodeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if len(strings.TrimSpace(string(line))) > 0 {
				malformed++
			}
			continue
		}
		if ev.SessionID != "" {
			res.SessionID = ev.SessionID
		}
		switch ev.Part.Type {
		case "text":
			if ev.Part.Synthetic || ev.Part.Ignored || ev.Part.Text == "" {
				break
			}
			if reply.Len() > 0 {
				reply.WriteString("\n\n")
			}
			reply.WriteString(ev.Part.Text)
			gotReply = true
			if cb.OnText != nil {
				cb.OnText(ev.Part.Text)
			}
		case "tool":
			if cb.OnTool != nil && ev.Part.Tool != "" {
				cb.OnTool(ToolUse{Name: ev.Part.Tool, FilePath: strInput(ev.Part.State.Input, "filePath")})
			}
		case "step-finish":
			// One step == one model request/response cycle, opencode's analog of
			// Claude's `assistant` event — count it so the closing frame reports a
			// real turn count instead of 0.
			res.Turns++
			// Each step reports its own tokens; the last step's are the turn's
			// (its input is the full context this turn ran against).
			res.Usage = Usage{
				Input:      ev.Part.Tokens.Input,
				Output:     ev.Part.Tokens.Output + ev.Part.Tokens.Reasoning,
				CacheRead:  ev.Part.Tokens.Cache.Read,
				CacheWrite: ev.Part.Tokens.Cache.Write,
			}
		}
		if ev.Type == "error" && failMsg == "" {
			if ev.Error.Data.Message != "" {
				failMsg = ev.Error.Data.Message
			} else if ev.Error.Message != "" {
				failMsg = ev.Error.Message
			}
		}
	}
	// Every error path still carries res.SessionID so a failed first turn stays
	// resumable (the caller adopts the id before checking the error).
	if err := sc.Err(); err != nil {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("read opencode stream: %w", err)
	}
	if failMsg != "" {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("opencode turn failed: %s", failMsg)
	}
	if !gotReply {
		if malformed > 0 {
			return TurnResult{SessionID: res.SessionID}, fmt.Errorf("opencode stream corrupted: no text message (%d malformed lines)", malformed)
		}
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("opencode stream ended without a text message")
	}
	res.Reply = reply.String()
	return res, nil
}

// parseOpencodeModels turns the stdout of `opencode models <provider>` into a model
// catalogue. Each non-empty line is a `provider/model` id (e.g.
// "ollama/qwen2.5-coder:7b"); the full line is the Flag handed to `-m`, and the
// alias is the tail after the provider so it reads as the bare model id
// ("qwen2.5-coder:7b"). Duplicate and blank lines are dropped. Spoken forms are
// auto-generated (punctuation → spaces) so voice-by-name has a chance; the
// reliable path stays voice-by-number, which needs no nicknames.
func parseOpencodeModels(stdout []byte) []Model {
	var models []Model
	seen := map[string]bool{}
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		slash := strings.IndexByte(line, '/')
		if slash < 0 {
			continue // not a provider/model id
		}
		alias := line[slash+1:]
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		models = append(models, Model{Alias: alias, Flag: line, Spoken: opencodeSpoken(alias)})
	}
	return models
}

// opencodeSpoken derives lightweight spoken forms for a discovered model alias so
// "use model <name>" has a shot at matching — the alias with its ":" / "-" / "."
// separators turned into spaces (e.g. "qwen2.5-coder:7b" → "qwen2 5 coder 7b").
// Returns nothing when that adds no new form (no separators).
func opencodeSpoken(alias string) []string {
	spaced := strings.NewReplacer(":", " ", "-", " ", ".", " ").Replace(alias)
	spaced = strings.Join(strings.Fields(spaced), " ")
	if spaced == "" || spaced == alias {
		return nil
	}
	return []string{spaced}
}

// strInput returns the string value at key in a tool part's input map, or "".
func strInput(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

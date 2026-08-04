package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// The Antigravity (agy) backend, self-contained: registry entry, per-turn command
// line, and the stream-json reader. Antigravity is Google's Gemini-powered agentic
// CLI. It drives one turn non-interactively via `agy --prompt`, and — like the
// other backends — emits a newline-delimited JSON event stream when asked for
// `--output-format stream-json`. That flag is undocumented in `agy --help` (only
// the binary's own flag table names "text, json, stream-json"), but it is real and
// carries everything the plain-text mode could not: the conversation id up front,
// per-step tool breadcrumbs, incremental assistant text, and token usage.
//
// The stream has three event kinds:
//
//	init        — once, first: conversation_id (agy's own, minted this run) + tool list
//	step_update — one per step transition: step_type user_input | agent_response |
//	              tool | checkpoint | unknown, with state ACTIVE then DONE. An
//	              agent_response carries text_delta chunks (accumulate per
//	              step_index); a tool carries tool_name + tool_info.parameters.
//	result      — once, last: status, the full response text with its paragraph
//	              breaks intact, num_turns, and the turn's summed usage.
//
// The result event's response is the authoritative reply — it preserves the
// message/paragraph structure that the old plain-text mode flattened into one
// blob, so the on-disk transcript reconstruction that used to repair that is gone.

// agyPrintTimeout caps agy's own non-interactive wait. Its default is 5m, too
// short for a real agentic turn; the gateway's context still governs cancellation,
// this just keeps agy from self-aborting a long turn before we do.
const agyPrintTimeout = "45m"

// antigravity builds the Antigravity backend entry. agy MINTS its own conversation
// id: --conversation can only *resume* an id agy already created ("Resume a previous
// conversation by ID"), and a caller-supplied uuid is rejected with "not found,
// ignoring --conversation flag". So this is a SelfAssignsID backend — the first turn
// runs with no --conversation, and the driver adopts the id agy announces in the
// stream's init event as the session id; every later turn resumes it with
// --conversation, which is what gives an antigravity session cross-turn memory.
// (That id is also the name of agy's on-disk brain directory, which is what the
// history reader replays — see session/antigravity_transcript.go.)
// agy ignores the process cwd and works in its own scratch project unless a
// workspace is named, so every turn
// passes the session's directory via --add-dir. Models are agy's display strings
// (what `agy --model` accepts verbatim), fronted by short spoken aliases.
func antigravity() *Agent {
	return &Agent{
		ID:            "antigravity",
		Name:          "Antigravity",
		Bin:           "agy",
		Transcript:    TranscriptAntigravity,
		SelfAssignsID: true,
		DefaultModel:  "gemini-pro",
		Models: []Model{
			// Flags are agy's exact `agy models` display strings — the value agy's
			// --model accepts. Aliases/spoken forms keep them sayable over voice; the
			// awkward parenthesised names are why ordinal selection ("use model 2")
			// matters here.
			{Alias: "gemini-pro", Flag: "Gemini 3.1 Pro (High)", Spoken: []string{"pro", "gemini pro", "gemini three pro"}},
			{Alias: "gemini-pro-low", Flag: "Gemini 3.1 Pro (Low)", Spoken: []string{"pro low", "pro fast"}},
			{Alias: "gemini-flash", Flag: "Gemini 3.5 Flash (High)", Spoken: []string{"flash", "gemini flash"}},
			{Alias: "gemini-flash-med", Flag: "Gemini 3.5 Flash (Medium)", Spoken: []string{"flash medium"}},
			{Alias: "gemini-flash-low", Flag: "Gemini 3.5 Flash (Low)", Spoken: []string{"flash low", "fast"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			// --conversation only RESUMES an id agy itself created, so it appears from
			// the second turn on, carrying the id the driver adopted after turn one.
			// The first turn passes none and lets agy mint one. agy works in its
			// private scratch project unless we name the workspace, so --add-dir
			// points it at the session directory.
			var args []string
			if s.Resume && s.SessionID != "" {
				args = append(args, "--conversation", s.SessionID)
			}
			if s.Dir != "" {
				args = append(args, "--add-dir", s.Dir)
			}
			if s.Bypass {
				args = append(args, "--dangerously-skip-permissions")
			}
			if m.Flag != "" {
				args = append(args, "--model", m.Flag)
			}
			args = append(args, "--print-timeout", agyPrintTimeout)
			// The machine-readable stream (see the file comment): the conversation id,
			// tool breadcrumbs, incremental text, and token usage all ride on it.
			args = append(args, "--output-format", "stream-json")
			// --prompt (alias of --print) triggers non-interactive mode and takes the
			// dictation as its value. The =form keeps a prompt that starts with "-"
			// from being misparsed as a flag.
			args = append(args, "--prompt="+s.Prompt)
			return args
		},
		ParseTurn: parseAgyStream,
	}
}

// agyEvent is the subset of one agy stream-json line we read. Exactly one of the
// three payloads is populated, selected by Event.
type agyEvent struct {
	Event string `json:"event"`
	// ConversationID rides at the TOP level of the init event, alongside "event" —
	// NOT inside the nested init object (which carries only cwd/tools/permission
	// mode). It repeats on every later event; the init one is the earliest sighting,
	// which is what makes a turn that dies mid-stream still resumable.
	ConversationID string       `json:"conversation_id"`
	StepUpdate     agyStep      `json:"step_update"`
	Result         agyResultMsg `json:"result"`
}

// agyStep is one step transition. A step is identified by StepIndex and reported
// at least twice (state ACTIVE then DONE); TextDelta chunks accumulate across
// those sightings, so a step's full text is the concatenation, not the last one.
type agyStep struct {
	StepIndex int    `json:"step_index"`
	State     string `json:"state"`
	StepType  string `json:"step_type"`
	TextDelta string `json:"text_delta"`
	ToolName  string `json:"tool_name"`
	ToolInfo  struct {
		Name       string                     `json:"name"`
		Parameters map[string]json.RawMessage `json:"parameters"`
	} `json:"tool_info"`
}

type agyResultMsg struct {
	ConversationID string   `json:"conversation_id"`
	Status         string   `json:"status"`
	Response       string   `json:"response"`
	NumTurns       int      `json:"num_turns"`
	Usage          agyUsage `json:"usage"`
}

// agyUsage is agy's per-turn token accounting. ThinkingTokens is reported
// alongside OutputTokens and is not added to it: on every step observed live
// output_tokens ≥ thinking_tokens, and the result event's output_tokens is the
// exact sum of the steps' — consistent with thinking being a labelled *portion*
// of output, not an extra charge. agy reports cache *reads* only — there is no
// cache-write counter.
type agyUsage struct {
	InputTokens     int `json:"input_tokens"`
	OutputTokens    int `json:"output_tokens"`
	ThinkingTokens  int `json:"thinking_tokens"`
	CacheReadTokens int `json:"cache_read_tokens"`
	TotalTokens     int `json:"total_tokens"`
}

// agyPathParams are the tool parameter keys agy uses to name the file a
// file-editing tool acts on, in preference order. Its tool schemas are not
// uniform — verified live, view_file takes AbsolutePath while write_to_file takes
// TargetFile — so we probe the known names instead of assuming one. The trailing
// names cover the other file tools' schemas (FilePath, NotebookPath); a tool that
// names no file (run_command, the searches) simply matches none.
var agyPathParams = []string{"AbsolutePath", "TargetFile", "FilePath", "NotebookPath"}

// agyToolPath pulls the acted-on file out of a tool step's parameters, or ""
// for a tool that names no file (run_command, search, …).
func agyToolPath(params map[string]json.RawMessage) string {
	for _, k := range agyPathParams {
		raw, ok := params[k]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// parseAgyStream consumes agy's newline-delimited stream-json for one turn: it
// adopts the conversation id from the init event, fans out a breadcrumb per tool
// step and each assistant message as it completes, and takes the final reply,
// turn count, and token usage from the result event. See the file comment for the
// event shapes.
//
// A step is reported ACTIVE then DONE, so tool breadcrumbs fire once (on the
// step's first sighting) and assistant text accumulates until the step is DONE,
// then flushes to OnText as one message — which is what gives the client the
// per-message paragraph structure.
func parseAgyStream(r io.Reader, cb TurnCallbacks) (TurnResult, error) {
	sc := NewLineScanner(r)

	var res TurnResult
	var gotResult bool
	var malformed int
	// text accumulates each in-flight agent_response step's deltas by step index;
	// seen marks steps already announced (tools) or flushed (text).
	text := map[int]string{}
	seenTool := map[int]bool{}
	flushed := map[int]bool{}
	for sc.Scan() {
		line := sc.Bytes()
		var ev agyEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			if len(strings.TrimSpace(string(line))) > 0 {
				malformed++
			}
			continue
		}
		switch ev.Event {
		case "init":
			if ev.ConversationID != "" {
				res.SessionID = ev.ConversationID
			}
		case "step_update":
			st := ev.StepUpdate
			switch st.StepType {
			case "agent_response":
				text[st.StepIndex] += st.TextDelta
				if st.State != "DONE" || flushed[st.StepIndex] {
					continue
				}
				flushed[st.StepIndex] = true
				// Every completed model cycle counts as a turn, including a
				// thinking-only step that emitted no prose.
				res.Turns++
				if msg := strings.TrimSpace(text[st.StepIndex]); msg != "" && cb.OnText != nil {
					cb.OnText(msg)
				}
				delete(text, st.StepIndex)
			case "tool":
				if seenTool[st.StepIndex] {
					continue
				}
				seenTool[st.StepIndex] = true
				name := st.ToolName
				if name == "" {
					name = st.ToolInfo.Name
				}
				if name != "" && cb.OnTool != nil {
					cb.OnTool(ToolUse{Name: name, FilePath: agyToolPath(st.ToolInfo.Parameters)})
				}
			}
		case "result":
			gotResult = true
			// The result repeats the id; adopt it if the init event was missed.
			if res.SessionID == "" && ev.Result.ConversationID != "" {
				res.SessionID = ev.Result.ConversationID
			}
			res.Reply = strings.TrimSpace(ev.Result.Response)
			res.Usage = Usage{
				Input:      ev.Result.Usage.InputTokens,
				Output:     ev.Result.Usage.OutputTokens,
				CacheRead:  ev.Result.Usage.CacheReadTokens,
				CacheWrite: 0, // agy reports no cache-write count
			}
			// agy's num_turns counts user messages (always 1 for a --prompt run),
			// not model cycles, so keep the agent_response count when we have one.
			if res.Turns == 0 {
				res.Turns = ev.Result.NumTurns
			}
			if st := ev.Result.Status; st != "" && st != "SUCCESS" {
				return TurnResult{SessionID: res.SessionID}, fmt.Errorf("agy turn failed: %s", st)
			}
		}
	}
	// Every error path still carries res.SessionID so a failed first turn remains
	// resumable (the caller adopts the id before checking the error).
	if err := sc.Err(); err != nil {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("read agy stream: %w", err)
	}
	if !gotResult {
		if malformed > 0 {
			return TurnResult{SessionID: res.SessionID}, fmt.Errorf("agy stream corrupted: no result event (%d malformed lines)", malformed)
		}
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("agy stream ended without a result")
	}
	if res.Reply == "" {
		return TurnResult{SessionID: res.SessionID}, fmt.Errorf("agy produced no response")
	}
	return res, nil
}

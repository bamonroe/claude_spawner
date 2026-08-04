package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestDefaultRegistryHasClaude(t *testing.T) {
	r := Default()
	c, ok := r.Get("claude")
	if !ok {
		t.Fatal("claude not registered")
	}
	if r.Default() != c {
		t.Errorf("default agent = %v, want claude", r.Default().ID)
	}
	if r.Resolve("") != c {
		t.Error("empty id should resolve to the default agent")
	}
	if r.Resolve("nope") != c {
		t.Error("unknown id should resolve to the default agent")
	}
}

func TestModelResolution(t *testing.T) {
	c, _ := Default().Get("claude")

	if m, ok := c.Model("sonnet"); !ok || m.Flag != "sonnet" {
		t.Errorf("sonnet resolved to %+v ok=%v", m, ok)
	}
	// Spoken form resolves to the canonical model.
	if m, ok := c.Model("fable five"); !ok || m.Alias != "fable" {
		t.Errorf("spoken 'fable five' resolved to %+v ok=%v", m, ok)
	}
	// Empty and unknown fall back to the default model (ok=false).
	if m, ok := c.Model(""); ok || m.Alias != c.DefaultModel {
		t.Errorf("empty resolved to %+v ok=%v, want default %q", m, ok, c.DefaultModel)
	}
	if m, ok := c.Model("gpt5"); ok || m.Alias != c.DefaultModel {
		t.Errorf("unknown resolved to %+v ok=%v, want default", m, ok)
	}
}

func TestCodexArgs(t *testing.T) {
	c, ok := Default().Get("codex")
	if !ok {
		t.Fatal("codex not registered")
	}
	if c.Bin != "codex" {
		t.Errorf("codex Bin = %q, want codex", c.Bin)
	}

	// First turn, default model: no id, model pinned, prompt after `--`.
	got := c.Args(TurnSpec{Prompt: "fix it", SessionID: "ignored", Resume: false, Model: "gpt-5.5", Bypass: true})
	want := []string{
		"exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-5.5", "--", "fix it",
	}
	if !slices.Equal(got, want) {
		t.Errorf("codex first-turn args\n got %v\nwant %v", got, want)
	}

	// Resume turn carries the id after `resume`; reasoning preset expands to -c args.
	got = c.Args(TurnSpec{Prompt: "-rf danger", SessionID: "thread-abc", Resume: true, Model: "gpt-5.5-high"})
	want = []string{
		"exec", "resume", "thread-abc", "--json", "--skip-git-repo-check",
		"-m", "gpt-5.5", "-c", "model_reasoning_effort=high", "--", "-rf danger",
	}
	if !slices.Equal(got, want) {
		t.Errorf("codex resume args\n got %v\nwant %v", got, want)
	}
}

func TestClaudeArgsMatchLegacyPlusModel(t *testing.T) {
	c, _ := Default().Get("claude")

	// First turn, no stored model → omit --model (legacy behavior), bypass on.
	got := c.Args(TurnSpec{Prompt: "hi", SessionID: "sid", Resume: false, Bypass: true})
	want := []string{
		"-p", "hi", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--dangerously-skip-permissions",
	}
	if !slices.Equal(got, want) {
		t.Errorf("first-turn args\n got %v\nwant %v", got, want)
	}

	// Explicit fable resolves to its full model id.
	got = c.Args(TurnSpec{Prompt: "hi", SessionID: "sid", Model: "fable"})
	want = []string{
		"-p", "hi", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--model", "claude-fable-5",
	}
	if !slices.Equal(got, want) {
		t.Errorf("fable args\n got %v\nwant %v", got, want)
	}

	// Resume turn, explicit sonnet, no bypass.
	got = c.Args(TurnSpec{Prompt: "more", SessionID: "sid", Resume: true, Model: "sonnet"})
	want = []string{
		"-p", "more", "--output-format", "stream-json", "--verbose",
		"--resume", "sid", "--model", "sonnet",
	}
	if !slices.Equal(got, want) {
		t.Errorf("resume args\n got %v\nwant %v", got, want)
	}

	// Operator ExtraArgs (SPAWNER_CLAUDE_EXTRA_ARGS) are appended verbatim, last.
	got = c.Args(TurnSpec{Prompt: "hi", SessionID: "sid", Bypass: true,
		ExtraArgs: []string{"--disable-slash-commands", "--setting-sources", "project"}})
	want = []string{
		"-p", "hi", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--dangerously-skip-permissions",
		"--disable-slash-commands", "--setting-sources", "project",
	}
	if !slices.Equal(got, want) {
		t.Errorf("extra-args\n got %v\nwant %v", got, want)
	}
}

func TestAntigravityArgs(t *testing.T) {
	a, ok := Default().Get("antigravity")
	if !ok {
		t.Fatal("antigravity not registered")
	}
	if a.Bin != "agy" {
		t.Errorf("antigravity Bin = %q, want agy", a.Bin)
	}
	if !a.SelfAssignsID {
		t.Error("antigravity mints its own conversation id (SelfAssignsID true)")
	}

	// A resuming turn carries the adopted conversation id; the workspace goes via
	// --add-dir, model pinned, prompt in =form so a leading-dash dictation can't be
	// misparsed as a flag.
	got := a.Args(TurnSpec{Prompt: "-rf be careful", SessionID: "conv-1", Resume: true, Dir: "/work", Model: "gemini-flash-low", Bypass: true})
	want := []string{
		"--conversation", "conv-1", "--add-dir", "/work",
		"--dangerously-skip-permissions", "--model", "Gemini 3.5 Flash (Low)",
		"--print-timeout", agyPrintTimeout, "--output-format", "stream-json",
		"--prompt=-rf be careful",
	}
	if !slices.Equal(got, want) {
		t.Errorf("antigravity args\n got %v\nwant %v", got, want)
	}

	// No Dir and no bypass: --add-dir and the skip-permissions flag are both omitted.
	got = a.Args(TurnSpec{Prompt: "hi", SessionID: "conv-2", Model: "gemini-pro"})
	if slices.Contains(got, "--add-dir") {
		t.Errorf("empty Dir should omit --add-dir, got %v", got)
	}
	if slices.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("no bypass should omit skip-permissions, got %v", got)
	}
	// A first turn (Resume false) must NOT pass --conversation: agy can only resume
	// an id it created itself, and rejects a caller-minted one.
	if slices.Contains(got, "--conversation") {
		t.Errorf("first turn should omit --conversation, got %v", got)
	}
}

// agyStream is a real two-message agy turn, trimmed to the fields we parse and
// keeping the live event shape: the id in init, a thinking-only agent_response,
// a tool step reported ACTIVE then DONE, and a prose agent_response whose text
// arrives split across its ACTIVE and DONE sightings.
const agyStream = `{"event":"init","conversation_id":"5e9cd790-1af2-4d2f-9cf7-050c03e5bab2","init":{"cwd":"/tmp","tools":["view_file"]}}
{"event":"step_update","step_update":{"step_index":0,"state":"DONE","step_type":"user_input"}}
{"event":"step_update","step_update":{"step_index":2,"state":"DONE","step_type":"agent_response","usage":{"input_tokens":9859,"output_tokens":695}}}
{"event":"step_update","step_update":{"step_index":3,"state":"ACTIVE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/tmp/hello.txt"}}}}
{"event":"step_update","step_update":{"step_index":3,"state":"DONE","step_type":"tool","tool_name":"view_file","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/tmp/hello.txt"},"output":"hi"}}}
{"event":"step_update","step_update":{"step_index":4,"state":"DONE","step_type":"tool","tool_name":"run_command","tool_info":{"name":"run_command","parameters":{"CommandLine":"ls -l"}}}}
{"event":"step_update","step_update":{"step_index":5,"state":"ACTIVE","step_type":"agent_response","text_delta":"Here is what "}}
{"event":"step_update","step_update":{"step_index":5,"state":"DONE","step_type":"agent_response","text_delta":"was done.\n"}}
{"event":"result","result":{"conversation_id":"5e9cd790-1af2-4d2f-9cf7-050c03e5bab2","status":"SUCCESS","response":"Here is what was done.\n","num_turns":1,"usage":{"input_tokens":12516,"output_tokens":1162,"thinking_tokens":948,"cache_read_tokens":16278}}}
`

func TestParseAgyStream(t *testing.T) {
	var texts []string
	var tools []ToolUse
	res, err := parseAgyStream(strings.NewReader(agyStream), TurnCallbacks{
		OnText: func(s string) { texts = append(texts, s) },
		OnTool: func(tu ToolUse) { tools = append(tools, tu) },
	})
	if err != nil {
		t.Fatalf("parseAgyStream: %v", err)
	}
	// The result event's response is the reply, paragraph breaks intact.
	if res.Reply != "Here is what was done." {
		t.Errorf("reply = %q", res.Reply)
	}
	// The init event's conversation id is adopted as the session id.
	if res.SessionID != "5e9cd790-1af2-4d2f-9cf7-050c03e5bab2" {
		t.Errorf("session id = %q", res.SessionID)
	}
	// A step's deltas accumulate across ACTIVE and DONE, and flush once, as one
	// message. The thinking-only step emits nothing.
	if len(texts) != 1 || texts[0] != "Here is what was done." {
		t.Errorf("OnText = %q, want one joined message", texts)
	}
	// Two agent_response steps completed = two model cycles, not agy's num_turns
	// (which counts the single user message).
	if res.Turns != 2 {
		t.Errorf("turns = %d, want 2 model cycles", res.Turns)
	}
	// A tool fires one breadcrumb despite two sightings; the file tool carries its
	// path, run_command carries none.
	want := []ToolUse{
		{Name: "view_file", FilePath: "/tmp/hello.txt"},
		{Name: "run_command"},
	}
	if !slices.Equal(tools, want) {
		t.Errorf("tools = %+v, want %+v", tools, want)
	}
	// Usage comes from the result's summed totals; thinking is a portion of
	// output, not an addition, and agy reports no cache writes.
	if got := (Usage{Input: 12516, Output: 1162, CacheRead: 16278}); res.Usage != got {
		t.Errorf("usage = %+v, want %+v", res.Usage, got)
	}
}

func TestParseAgyStreamErrors(t *testing.T) {
	// A stream that ends before the result event is a failed turn — but it still
	// carries the id, so the caller can resume rather than re-create.
	truncated := strings.SplitAfter(agyStream, "\n")[0]
	res, err := parseAgyStream(strings.NewReader(truncated), TurnCallbacks{})
	if err == nil {
		t.Error("truncated stream should error")
	}
	if res.SessionID != "5e9cd790-1af2-4d2f-9cf7-050c03e5bab2" {
		t.Errorf("failed turn must still report the id, got %q", res.SessionID)
	}

	// A non-SUCCESS result is a failed turn, reported with agy's status.
	failed := `{"event":"init","conversation_id":"abc","init":{"cwd":"/tmp"}}
{"event":"result","result":{"status":"ERROR","response":""}}
`
	res, err = parseAgyStream(strings.NewReader(failed), TurnCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "ERROR") {
		t.Errorf("failed status should error with the status, got %v", err)
	}
	if res.SessionID != "abc" {
		t.Errorf("failed turn must still report the id, got %q", res.SessionID)
	}

	// A clean stream whose result carries no text is a failed turn.
	empty := `{"event":"result","result":{"status":"SUCCESS","response":"  "}}
`
	if _, err := parseAgyStream(strings.NewReader(empty), TurnCallbacks{}); err == nil {
		t.Error("empty response should error")
	}
}

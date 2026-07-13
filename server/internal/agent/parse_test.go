package agent

import (
	"strings"
	"testing"
)

// TestParseClaudeStreamStreamsProse checks that each assistant text message is
// delivered live via OnText (in order), tool_use blocks go to OnTool, and the
// final result is returned — so the caller can show Claude's prose as it lands
// instead of all at once at turn end.
func TestParseClaudeStreamStreamsProse(t *testing.T) {
	const stream = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Let me look."}]}}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/tmp/foo.go"}}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Found it."}]}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1783173600,"rateLimitType":"five_hour","isUsingOverage":false}}
{"type":"result","subtype":"success","result":"Found it.","session_id":"x","usage":{"input_tokens":12,"output_tokens":7,"cache_creation_input_tokens":1000,"cache_read_input_tokens":24000}}
`
	var texts []string
	var tools []ToolUse
	var limits []RateLimit
	res, err := parseClaudeStream(strings.NewReader(stream), TurnCallbacks{
		OnTool:      func(t ToolUse) { tools = append(tools, t) },
		OnText:      func(s string) { texts = append(texts, s) },
		OnRateLimit: func(rl RateLimit) { limits = append(limits, rl) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "Found it." {
		t.Errorf("reply = %q, want %q", res.Reply, "Found it.")
	}
	if res.SessionID != "" {
		t.Errorf("SessionID = %q, want empty (Claude takes a caller-supplied id)", res.SessionID)
	}
	if want := (Usage{Input: 12, Output: 7, CacheWrite: 1000, CacheRead: 24000}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	if want := (RateLimit{Status: "allowed", ResetsAt: 1783173600, Type: "five_hour"}); len(limits) != 1 || limits[0] != want {
		t.Errorf("rate limits = %+v, want one %+v", limits, want)
	}
	if want := []string{"Let me look.", "Found it."}; strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("streamed texts = %v, want %v", texts, want)
	}
	if len(tools) != 1 || tools[0].Name != "Read" || tools[0].FilePath != "/tmp/foo.go" {
		t.Errorf("tools = %+v, want one Read of /tmp/foo.go", tools)
	}
}

// TestParseCodexStream feeds the real `codex exec --json` event shapes (captured
// from a live run): thread.started carries the id, a step item becomes a tool
// breadcrumb, agent_message is the reply, turn.completed carries usage.
func TestParseCodexStream(t *testing.T) {
	const stream = `{"type":"thread.started","thread_id":"019f4971-0a8c-74a0-a384-d833e64fd77e"}
{"type":"turn.started"}
{"type":"item.completed","item":{"id":"i0","type":"command_execution","command":"ls"}}
{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":7,"reasoning_output_tokens":3}}
`
	var texts []string
	var tools []ToolUse
	res, err := parseCodexStream(strings.NewReader(stream), TurnCallbacks{
		OnTool: func(t ToolUse) { tools = append(tools, t) },
		OnText: func(s string) { texts = append(texts, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reply != "done" {
		t.Errorf("reply = %q, want %q", res.Reply, "done")
	}
	if res.SessionID != "019f4971-0a8c-74a0-a384-d833e64fd77e" {
		t.Errorf("SessionID = %q", res.SessionID)
	}
	// Output folds in reasoning tokens (7+3); cached maps to CacheRead.
	if want := (Usage{Input: 100, Output: 10, CacheRead: 40}); res.Usage != want {
		t.Errorf("usage = %+v, want %+v", res.Usage, want)
	}
	if want := []string{"done"}; strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("texts = %v, want %v", texts, want)
	}
	if len(tools) != 1 || tools[0].Name != "command_execution" {
		t.Errorf("tools = %+v, want one command_execution", tools)
	}
}

// TestParseCodexStreamFailure confirms a turn.failed event surfaces as an error
// while the thread_id (seen first) is still returned in the TurnResult, so the
// failed first turn remains resumable rather than getting re-created.
func TestParseCodexStreamFailure(t *testing.T) {
	const stream = `{"type":"thread.started","thread_id":"tid-9"}
{"type":"turn.started"}
{"type":"turn.failed","error":{"message":"model not supported"}}
`
	res, err := parseCodexStream(strings.NewReader(stream), TurnCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "model not supported") {
		t.Fatalf("err = %v, want it to mention the failure", err)
	}
	if res.SessionID != "tid-9" {
		t.Errorf("SessionID = %q, want tid-9 even on failure", res.SessionID)
	}
}

// TestParseClaudeStreamReportsCorruption confirms a stream that truncates
// mid-flight (garbage lines, no result event) surfaces the malformed-line count
// instead of a bare "no result" — so a corrupted claude stdout is diagnosable.
func TestParseClaudeStreamReportsCorruption(t *testing.T) {
	const stream = `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
not json at all
{"type":"assistant","message":{"content":[{"typ` // truncated line
	_, err := parseClaudeStream(strings.NewReader(stream), TurnCallbacks{})
	if err == nil {
		t.Fatal("expected an error on a resultless stream")
	}
	if !strings.Contains(err.Error(), "corrupted") || !strings.Contains(err.Error(), "2 malformed") {
		t.Errorf("err = %v, want it to mention 'corrupted' and '2 malformed lines'", err)
	}
}

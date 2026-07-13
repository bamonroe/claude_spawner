package session

import (
	"encoding/json"
	"testing"
)

// realExport is a trimmed but real-shaped `opencode export` payload: a user turn,
// an assistant turn that was tool-only (no prose), then an assistant turn with
// prose. Token accounting lives on step-finish parts; the session-level
// info.tokens is intentionally larger (summed across turns) to prove the reader
// ignores it in favor of the last step-finish.
const realExport = `{
  "info": { "id": "ses_x", "tokens": { "input": 4831, "output": 50 } },
  "messages": [
    { "info": { "role": "user", "time": { "created": 1783980863951 } },
      "parts": [ { "type": "text", "text": "reply hi then stop" } ] },
    { "info": { "role": "assistant", "time": { "created": 1783980865000 } },
      "parts": [
        { "type": "step-start" },
        { "type": "tool", "tool": "task" },
        { "type": "step-finish", "tokens": { "input": 2050, "output": 32, "reasoning": 0, "cache": { "read": 0, "write": 0 } } }
      ] },
    { "info": { "role": "assistant", "time": { "created": 1783980870000 } },
      "parts": [
        { "type": "step-start" },
        { "type": "text", "text": "Hi." },
        { "type": "text", "text": "(internal)", "synthetic": true },
        { "type": "step-finish", "tokens": { "input": 2781, "output": 18, "reasoning": 2, "cache": { "read": 40, "write": 5 } } }
      ] }
  ]
}`

func parseExport(t *testing.T) opencodeExport {
	t.Helper()
	var ex opencodeExport
	if err := json.Unmarshal([]byte(realExport), &ex); err != nil {
		t.Fatal(err)
	}
	return ex
}

// TestExportMessages checks the export → conversation mapping: user + assistant
// roles map through (assistant → "claude"), a tool-only assistant turn is dropped
// from the replay, synthetic text is skipped, and the surviving assistant turn
// carries its last step-finish usage (reasoning folded into Output).
func TestExportMessages(t *testing.T) {
	msgs := exportMessages(parseExport(t))
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (tool-only assistant turn dropped): %+v", len(msgs), msgs)
	}
	// Index is assigned by readTranscriptChain across the whole chain, not here.
	if msgs[0].Role != "user" || msgs[0].Text != "reply hi then stop" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[0].Usage != nil {
		t.Errorf("user turn should carry no usage, got %+v", msgs[0].Usage)
	}
	if msgs[1].Role != "claude" || msgs[1].Text != "Hi." {
		t.Errorf("msg1 = %+v (synthetic text should be skipped)", msgs[1])
	}
	if msgs[1].Ts != 1783980870 {
		t.Errorf("msg1 ts = %d, want ms→s conversion 1783980870", msgs[1].Ts)
	}
	want := Usage{Input: 2781, Output: 20, CacheRead: 40, CacheWrite: 5}
	if msgs[1].Usage == nil || *msgs[1].Usage != want {
		t.Errorf("msg1 usage = %+v, want %+v", msgs[1].Usage, want)
	}
}

// TestExportContext confirms the context snapshot is the LAST step-finish's
// tokens (the newest turn's full prompt), not the summed session-level
// info.tokens — so the reattach context meter matches the live one.
func TestExportContext(t *testing.T) {
	cx := exportContext(parseExport(t))
	if cx == nil {
		t.Fatal("want a context snapshot")
	}
	want := Usage{Input: 2781, Output: 20, CacheRead: 40, CacheWrite: 5}
	if cx.Usage != want {
		t.Errorf("context usage = %+v, want the last step-finish %+v (not summed info.tokens)", cx.Usage, want)
	}
	if cx.At != 1783980870 {
		t.Errorf("context at = %d, want 1783980870", cx.At)
	}
}

// TestValidOpencodeID gates the ids interpolated into remote shell commands: only
// `ses_`+alphanumerics pass, so a malformed or injection-bearing id is rejected.
func TestValidOpencodeID(t *testing.T) {
	good := []string{"ses_0a2744e61ffe86lSFnc0BVot51", "ses_abc123"}
	bad := []string{"", "ses_", "abc", "ses_bad-id", "ses_a b", "ses_a;rm -rf", "ses_a/b"}
	for _, id := range good {
		if !validOpencodeID(id) {
			t.Errorf("validOpencodeID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if validOpencodeID(id) {
			t.Errorf("validOpencodeID(%q) = true, want false", id)
		}
	}
}

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// A minimal two-turn Codex rollout: session_meta + turn scaffolding are ignored;
// user_message/agent_message carry the prose and token_count the context usage.
const codexRollout = `{"type":"session_meta","timestamp":"2026-07-10T00:19:43.925Z","payload":{"session_id":"019f4964-fe35-7e92-b5e7-dc16a6b10658","cwd":"/tmp"}}
{"type":"event_msg","timestamp":"2026-07-10T00:19:44.000Z","payload":{"type":"task_started"}}
{"type":"event_msg","timestamp":"2026-07-10T00:19:45.000Z","payload":{"type":"user_message","message":"Reply with exactly the word: pong"}}
{"type":"event_msg","timestamp":"2026-07-10T00:19:46.000Z","payload":{"type":"agent_message","message":"pong"}}
{"type":"event_msg","timestamp":"2026-07-10T00:19:47.000Z","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":12259,"cached_input_tokens":4992,"output_tokens":5},"last_token_usage":{"input_tokens":12259,"cached_input_tokens":4992,"output_tokens":5,"reasoning_output_tokens":0}}}}
{"type":"response_item","timestamp":"2026-07-10T00:19:47.500Z","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<permissions instructions>"}]}}
{"type":"event_msg","timestamp":"2026-07-10T00:20:00.000Z","payload":{"type":"user_message","message":"What word did you just say?"}}
{"type":"event_msg","timestamp":"2026-07-10T00:20:01.000Z","payload":{"type":"agent_message","message":"pong"}}
{"type":"event_msg","timestamp":"2026-07-10T00:20:02.000Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":14328,"cached_input_tokens":4992,"output_tokens":6,"reasoning_output_tokens":2}}}}
`

// TestCodexReadTranscript confirms a Codex rollout replays as ordered user/claude
// prose (developer scaffolding + session_meta skipped), each claude turn badged
// with that turn's context usage (fresh input split from the cached prefix).
func TestCodexReadTranscript(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := codexFS{}
	msgs, err := fs.readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		role, text string
	}{
		{"user", "Reply with exactly the word: pong"},
		{"claude", "pong"},
		{"user", "What word did you just say?"},
		{"claude", "pong"},
	}
	if len(msgs) != len(want) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(want), msgs)
	}
	for i, w := range want {
		if msgs[i].Role != w.role || msgs[i].Text != w.text || msgs[i].Index != i {
			t.Errorf("msg %d = %+v, want role=%s text=%q index=%d", i, msgs[i], w.role, w.text, i)
		}
	}
	if msgs[0].Ts != 1783642785 {
		t.Errorf("user Ts = %d, want 1783642785", msgs[0].Ts)
	}
	// First claude turn: 12259 input incl. 4992 cached → 7267 fresh + 4992 cached.
	if u := msgs[1].Usage; u == nil || *u != (Usage{Input: 7267, Output: 5, CacheRead: 4992}) {
		t.Errorf("claude[1] usage = %+v, want {7267 5 0 4992}", u)
	}
	// User turns never carry a badge.
	if msgs[0].Usage != nil || msgs[2].Usage != nil {
		t.Error("user turns should carry no usage")
	}
}

// TestCodexLastContextUsage confirms the snapshot is the newest turn's
// last_token_usage (current context occupancy), not the running total.
func TestCodexLastContextUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(codexRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := codexFS{}
	cx := fs.lastUsageInFile(path)
	if cx == nil {
		t.Fatal("want a snapshot, got nil")
	}
	// Last token_count: 14328 input incl. 4992 cached → 9336 fresh; output 6+2.
	if cx.Usage != (Usage{Input: 9336, Output: 8, CacheRead: 4992}) {
		t.Errorf("usage = %+v, want {9336 8 0 4992}", cx.Usage)
	}
	if cx.At != 1783642802 {
		t.Errorf("At = %d, want 1783642802", cx.At)
	}
}

// TestCodexFindByID confirms a thread_id resolves to its rollout by the trailing
// UUID in the date-partitioned session tree.
func TestCodexFindByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "019f4964-fe35-7e92-b5e7-dc16a6b10658"
	sub := filepath.Join(home, ".codex", "sessions", "2026", "07", "09")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sub, "rollout-2026-07-09T20-19-43-"+id+".jsonl")
	if err := os.WriteFile(path, []byte(codexRollout), 0o600); err != nil {
		t.Fatal(err)
	}
	fs := codexFS{}
	if got := fs.findByID(id); got != path {
		t.Errorf("findByID = %q, want %q", got, path)
	}
	if got := fs.findByID("no-such-id"); got != "" {
		t.Errorf("findByID(missing) = %q, want empty", got)
	}
}

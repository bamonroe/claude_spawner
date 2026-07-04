package session

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionID(t *testing.T) {
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidRe.MatchString(a) {
		t.Errorf("not a v4 uuid: %q", a)
	}
	b, _ := NewSessionID()
	if a == b {
		t.Errorf("expected distinct ids, got %q twice", a)
	}
}

// TestParseStreamStreamsProse checks that each assistant text message is
// delivered live via onText (in order), tool_use blocks go to onTool, and the
// final result is returned — so the caller can show Claude's prose as it lands
// instead of all at once at turn end.
func TestParseStreamStreamsProse(t *testing.T) {
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
	reply, usage, err := parseStream(strings.NewReader(stream),
		func(t ToolUse) { tools = append(tools, t) },
		func(s string) { texts = append(texts, s) },
		func(rl RateLimit) { limits = append(limits, rl) },
	)
	if err != nil {
		t.Fatal(err)
	}
	if reply != "Found it." {
		t.Errorf("reply = %q, want %q", reply, "Found it.")
	}
	if want := (Usage{Input: 12, Output: 7, CacheWrite: 1000, CacheRead: 24000}); usage != want {
		t.Errorf("usage = %+v, want %+v", usage, want)
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

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %d", len(got))
	}

	rec := &Session{Name: "claude-claude", Dir: "/data/claude_claude", SessionID: "id-1"}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}

	// Reopen and confirm persistence.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get("claude-claude")
	if got == nil || got.Dir != "/data/claude_claude" || got.SessionID != "id-1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := s2.Delete("claude-claude"); err != nil {
		t.Fatal(err)
	}
	if s2.Get("claude-claude") != nil {
		t.Error("expected record gone after delete")
	}
}

func TestTranscriptIDs(t *testing.T) {
	s := &Session{SessionID: "cur"}
	if got := s.TranscriptIDs(); len(got) != 1 || got[0] != "cur" {
		t.Fatalf("TranscriptIDs() = %v, want [cur]", got)
	}
	// A cleared session lists retired ids oldest-first, then the current one, so
	// the concatenated history reads in chronological order.
	s.PriorIDs = []string{"old1", "old2"}
	if got := strings.Join(s.TranscriptIDs(), ","); got != "old1,old2,cur" {
		t.Errorf("TranscriptIDs() = %q, want %q", got, "old1,old2,cur")
	}
}

// TestReadTranscriptParsesTimestamp confirms a transcript line's ISO-8601
// timestamp is surfaced as unix seconds on Message.Ts (0 when absent).
func TestReadTranscriptParsesTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","timestamp":"2026-07-04T11:51:00Z","message":{"content":"hi"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Ts != 1783165860 {
		t.Errorf("user Ts = %d, want 1783165860", msgs[0].Ts)
	}
	if msgs[1].Ts != 0 {
		t.Errorf("timestamp-less line should have Ts 0, got %d", msgs[1].Ts)
	}
}

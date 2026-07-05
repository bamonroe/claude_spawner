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

// TestParseStreamReportsCorruption confirms a stream that truncates mid-flight
// (garbage lines, no result event) surfaces the malformed-line count instead of
// a bare "no result" — so a corrupted claude stdout is diagnosable.
func TestParseStreamReportsCorruption(t *testing.T) {
	const stream = `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}
not json at all
{"type":"assistant","message":{"content":[{"typ` // truncated line
	_, _, err := parseStream(strings.NewReader(stream), nil, nil, nil)
	if err == nil {
		t.Fatal("expected an error on a resultless stream")
	}
	if !strings.Contains(err.Error(), "corrupted") || !strings.Contains(err.Error(), "2 malformed") {
		t.Errorf("err = %v, want it to mention 'corrupted' and '2 malformed lines'", err)
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

// TestLastUsageInFile confirms the context snapshot is the newest assistant turn
// with a non-zero prompt (input + cache), carrying its usage and timestamp, and
// that usage-less lines (user turns, tool-only sub-turns) are skipped.
func TestLastUsageInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	lines := `{"type":"user","message":{"content":"hi"}}
{"type":"assistant","timestamp":"2026-07-04T11:00:00Z","message":{"content":[{"type":"text","text":"a"}],"usage":{"input_tokens":5,"output_tokens":10,"cache_creation_input_tokens":20,"cache_read_input_tokens":100}}}
{"type":"assistant","timestamp":"2026-07-04T11:01:00Z","message":{"content":[{"type":"text","text":"b"}],"usage":{"input_tokens":2,"output_tokens":7,"cache_creation_input_tokens":30,"cache_read_input_tokens":200}}}
{"type":"assistant","message":{"content":[{"type":"tool_use"}],"usage":{"input_tokens":0,"output_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	cx := lastUsageInFile(path)
	if cx == nil {
		t.Fatal("want a snapshot, got nil")
	}
	// The last non-zero line wins (the trailing zero-usage line is skipped).
	want := Usage{Input: 2, Output: 7, CacheWrite: 30, CacheRead: 200}
	if cx.Usage != want {
		t.Errorf("usage = %+v, want %+v", cx.Usage, want)
	}
	if cx.At != 1783162860 {
		t.Errorf("At = %d, want 1783162860", cx.At)
	}
	// A transcript with no usage-bearing assistant line yields nil.
	empty := filepath.Join(dir, "e.jsonl")
	if err := os.WriteFile(empty, []byte(`{"type":"user","message":{"content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := lastUsageInFile(empty); got != nil {
		t.Errorf("want nil for usage-less transcript, got %+v", got)
	}
}

// TestReadTranscriptCarriesTurnUsage confirms per-message usage is attached only
// to the final assistant line of each turn (matching the live closing-message
// badge): an intermediate assistant line in a multi-line turn carries none, and
// user turns never do.
func TestReadTranscriptCarriesTurnUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	// Turn 1: user, then two assistant lines (text + tool-call-then-text) → only
	// the second keeps usage. Turn 2: user, one assistant line → it keeps usage.
	lines := `{"type":"user","message":{"content":"go"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"working"}],"usage":{"input_tokens":1,"cache_read_input_tokens":50}}}
{"type":"assistant","message":{"content":[{"type":"text","text":"done"}],"usage":{"input_tokens":3,"output_tokens":9,"cache_creation_input_tokens":10,"cache_read_input_tokens":80}}}
{"type":"user","message":{"content":"again"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":4,"cache_read_input_tokens":90}}}
`
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	msgs, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d", len(msgs))
	}
	if msgs[0].Usage != nil || msgs[3].Usage != nil {
		t.Error("user messages should carry no usage")
	}
	if msgs[1].Usage != nil {
		t.Error("intermediate assistant line should have usage cleared")
	}
	if msgs[2].Usage == nil || msgs[2].Usage.CacheRead != 80 {
		t.Errorf("final assistant line of turn 1 should keep usage, got %+v", msgs[2].Usage)
	}
	if msgs[4].Usage == nil || msgs[4].Usage.CacheRead != 90 {
		t.Errorf("lone assistant line of turn 2 should keep usage, got %+v", msgs[4].Usage)
	}
}

// TestTranscriptCacheInvalidatesOnChange confirms the per-file parse cache is
// self-invalidating: reading primes the cache, and appending a turn (which grows
// the file) must yield the fresh content on the next read, not the stale cached
// parse. Both the message list and the usage snapshot are covered.
func TestTranscriptCacheInvalidatesOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	first := `{"type":"user","message":{"content":"hi"}}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := ReadTranscript(path); len(msgs) != 1 { // prime the message cache
		t.Fatalf("first read: want 1 message, got %d", len(msgs))
	}
	if snap := lastUsageInFile(path); snap != nil { // prime the snapshot cache (no usage yet)
		t.Fatalf("first snapshot: want nil, got %+v", snap)
	}

	// Append a usage-bearing assistant turn; the file grows, so a stat-keyed cache
	// must miss and re-parse.
	extra := `{"type":"assistant","timestamp":"2026-07-04T11:00:00Z","message":{"content":[{"type":"text","text":"yo"}],"usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":50}}}` + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		t.Fatal(err)
	}
	f.Close()

	msgs, err := ReadTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("after append: want 2 messages (cache must invalidate), got %d", len(msgs))
	}
	snap := lastUsageInFile(path)
	if snap == nil || snap.Usage.CacheRead != 50 {
		t.Errorf("after append: snapshot = %+v, want CacheRead 50", snap)
	}
}

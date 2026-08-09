package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// writeTranscript writes a Claude transcript for id under HOME's projects dir
// with one user/assistant pair per entry in pairs, so the driver's display read
// finds a log of len(pairs)*2 rows.
func writeTranscript(t *testing.T, home, id string, pairs ...string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var body string
	for _, p := range pairs {
		body += `{"type":"user","message":{"content":"ask-` + p + `"}}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"text","text":"say-` + p + `"}]}}` + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHistoryDeltaOnValidatedPrefix drives the delta history wire end to end: a
// first `history` reply hands the app a prefix digest; after a turn appends rows,
// echoing that digest back as have_prefix returns ONLY the appended rows with
// delta=true; a stale prefix falls back to a full page.
func TestHistoryDeltaOnValidatedPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	const id = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	writeTranscript(t, home, id, "one")

	ts, root, gw := newTestServerGW(t, nil)
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := &session.Session{Name: "proj", Dir: dir, SessionID: id, Target: session.TargetHost, Host: session.LocalHost}
	// No chain-signature memo: this test appends to the transcript and re-reads it
	// within the default freshness window, which would report it unchanged.
	gw.driver.SigTTL(0)
	if err := gw.store.Put(rec); err != nil {
		t.Fatal(err)
	}

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// First fetch: a normal full page that also carries the prefix digest.
	send(t, ws, map[string]any{"type": "history", "session_id": id})
	h := readUntil(t, ws, "history")
	if h["delta"] == true {
		t.Fatal("a first fetch with no have_prefix must not be a delta")
	}
	if n := len(h["messages"].([]any)); n != 2 {
		t.Fatalf("first page has %d messages, want 2", n)
	}
	prefix, _ := h["prefix_hash"].(string)
	if prefix == "" {
		t.Fatal("no prefix_hash on the page reply")
	}
	if pc, _ := h["prefix_count"].(float64); int(pc) != 2 {
		t.Fatalf("prefix_count = %v, want 2", h["prefix_count"])
	}

	// A turn lands: two more rows appended to the same transcript.
	writeTranscript(t, home, id, "one", "two")

	send(t, ws, map[string]any{"type": "history", "session_id": id,
		"have_prefix": prefix, "have_prefix_count": 2})
	h = readUntil(t, ws, "history")
	if h["delta"] != true {
		t.Fatalf("a matching prefix must yield a delta, got %v", h)
	}
	if bc, _ := h["base_count"].(float64); int(bc) != 2 {
		t.Fatalf("base_count = %v, want 2", h["base_count"])
	}
	msgs, _ := h["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("delta carries %d messages, want only the 2 appended rows", len(msgs))
	}
	if got := msgs[0].(map[string]any)["text"]; got != "ask-two" {
		t.Fatalf("delta starts at %v, want the first appended row", got)
	}
	if pc, _ := h["prefix_count"].(float64); int(pc) != 4 {
		t.Fatalf("delta prefix_count = %v, want 4 (the new full log)", h["prefix_count"])
	}

	// A stale prefix falls back to today's full page.
	send(t, ws, map[string]any{"type": "history", "session_id": id,
		"have_prefix": "stale", "have_prefix_count": 2})
	h = readUntil(t, ws, "history")
	if h["delta"] == true {
		t.Fatal("a stale prefix must not yield a delta")
	}
	if n := len(h["messages"].([]any)); n != 4 {
		t.Fatalf("fallback page has %d messages, want the full 4", n)
	}
}

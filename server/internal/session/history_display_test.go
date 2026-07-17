package session

import (
	"path/filepath"
	"testing"
)

// writeClaudeTranscript writes a minimal two-message (user + assistant) Claude
// transcript for id under a temp HOME's projects dir, so claudeFS.findByID locates
// it. userText/asstText become the prose of the two messages.
func writeClaudeTranscript(t *testing.T, home, id, userText, asstText string) {
	t.Helper()
	proj := filepath.Join(home, ".claude", "projects", "-data")
	path := filepath.Join(proj, id+".jsonl")
	body := `{"type":"user","message":{"content":"` + userText + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + asstText + `"}]}}` + "\n"
	writeFile(t, path, body)
}

// TestReadDisplayHistory_MergesArchivedThenCurrent confirms the cross-backend
// display read concatenates each archived History segment ahead of the current
// backend's chain, in order, re-indexed contiguously so pagination stays stable.
func TestReadDisplayHistory_MergesArchivedThenCurrent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	archived := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	current := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	writeClaudeTranscript(t, home, archived, "old-user", "old-assistant")
	writeClaudeTranscript(t, home, current, "new-user", "new-assistant")

	rec := &Session{
		Name:      "s",
		SessionID: current, // default (empty) agent → Claude reader for both segments
		History:   []HistorySegment{{IDs: []string{archived}}},
	}
	d := NewDriver()

	msgs, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	wantText := []string{"old-user", "old-assistant", "new-user", "new-assistant"}
	if len(msgs) != len(wantText) {
		t.Fatalf("got %d messages, want %d: %+v", len(msgs), len(wantText), msgs)
	}
	for i, w := range wantText {
		if msgs[i].Text != w {
			t.Errorf("msg[%d].Text = %q, want %q", i, msgs[i].Text, w)
		}
		if msgs[i].Index != i {
			t.Errorf("msg[%d].Index = %d, want %d (re-index must be contiguous)", i, msgs[i].Index, i)
		}
	}

	// With no archived history the read equals just the current chain — the exact
	// pre-split behavior, so old sessions are unaffected.
	rec.History = nil
	plain, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 2 || plain[0].Text != "new-user" {
		t.Fatalf("no-history read = %+v, want just the current chain", plain)
	}
}

// TestArchiveSegment_IDsByBackend confirms the archived segment captures the current
// backend's display ids: the session_id chain for Claude/Codex, but antigravity's own
// brain ids (agy ignores our session_id, so its history lives under AgyBrainIDs).
func TestArchiveSegment_IDsByBackend(t *testing.T) {
	d := NewDriver()

	claude := &Session{Agent: "", Host: "", SessionID: "cur", PriorIDs: []string{"old"}}
	seg := d.ArchiveSegment(claude)
	if got, want := seg.IDs, []string{"old", "cur"}; !equalStrings(got, want) {
		t.Errorf("claude segment IDs = %v, want %v (prior ids + current)", got, want)
	}

	agy := &Session{Agent: "antigravity", SessionID: "cur", AgyBrainIDs: []string{"g1", "g2"}}
	seg = d.ArchiveSegment(agy)
	if got, want := seg.IDs, []string{"g1", "g2"}; !equalStrings(got, want) {
		t.Errorf("antigravity segment IDs = %v, want %v (brain ids, not session_id)", got, want)
	}
	if seg.Agent != "antigravity" {
		t.Errorf("segment Agent = %q, want antigravity", seg.Agent)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

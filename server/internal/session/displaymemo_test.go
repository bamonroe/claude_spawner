package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// transcriptPathFor is where writeClaudeTranscript puts a transcript.
func transcriptPathFor(home, id string) string {
	return filepath.Join(home, ".claude", "projects", "-data", id+".jsonl")
}

// rewriteSameStat replaces a transcript's contents with body while restoring its
// size and mtime, so every freshness signature still reports it as unchanged. It's
// the only way to tell a memo hit from a re-read: identical signature, different
// bytes on disk — whatever comes back is what was remembered.
func rewriteSameStat(t *testing.T, path, body string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) != fi.Size() {
		t.Fatalf("rewrite must preserve size: got %d, want %d", len(body), fi.Size())
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
}

// dropFileCaches empties the process-wide transcript parse caches, so the only
// thing that can still answer a read without touching disk is the display memo
// under test. Without this the resumable claudeParse cache would serve the same
// stale bytes and the assertions below would pass for the wrong reason.
func dropFileCaches(t *testing.T) {
	t.Helper()
	claudeParseMu.Lock()
	claudeParseCache = map[string]*claudeParse{}
	claudeParseMu.Unlock()
	transcriptCacheMu.Lock()
	transcriptCache = map[string]transcriptCacheEntry{}
	transcriptCacheMu.Unlock()
}

func claudeTranscriptBody(userText, asstText string) string {
	return `{"type":"user","message":{"content":"` + userText + `"}}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"` + asstText + `"}]}}` + "\n"
}

// TestDisplayMemo_ServesUnchangedChain confirms a second read of a chain nothing
// has touched comes from the memo rather than the disk — the case every history
// page-back hits, since `before != nil` skips the digest fast path entirely.
func TestDisplayMemo_ServesUnchangedChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	writeClaudeTranscript(t, home, id, "user-one", "asst-one")

	d := NewDriver()
	rec := &Session{Name: "s", SessionID: id}
	if _, err := d.ReadDisplayHistory(rec); err != nil {
		t.Fatal(err)
	}
	// Same size, same mtime, different bytes: a real read would see "user-two".
	rewriteSameStat(t, transcriptPathFor(home, id), claudeTranscriptBody("user-two", "asst-two"))
	dropFileCaches(t)

	msgs, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Text != "user-one" {
		t.Fatalf("unchanged chain re-read from disk; got %+v", msgs)
	}
}

// TestDisplayMemo_ArchivedSegmentSurvivesCurrentGrowth is the point of the
// per-segment level: a session that keeps producing output invalidates the
// whole-log memo on every turn, but its archived segments belong to a backend it
// switched away from and can never change, so they must not be re-read.
func TestDisplayMemo_ArchivedSegmentSurvivesCurrentGrowth(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	archived := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	current := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	writeClaudeTranscript(t, home, archived, "old-user", "old-asst")
	writeClaudeTranscript(t, home, current, "new-user", "new-asst")

	d := NewDriver()
	// This test appends to the transcript and re-reads within microseconds; the
	// chainSig memo would (correctly, per its TTL contract) still report the chain
	// unchanged. A real turn outlives the TTL, so switch the memo off to test the
	// display memo's own invalidation.
	d.sigTTL = 0
	rec := &Session{Name: "s", SessionID: current, History: []HistorySegment{{IDs: []string{archived}}}}
	if _, err := d.ReadDisplayHistory(rec); err != nil {
		t.Fatal(err)
	}
	rewriteSameStat(t, transcriptPathFor(home, archived), claudeTranscriptBody("XXX-user", "XXX-asst"))
	dropFileCaches(t)

	// Grow the current chain, the way a turn does: the whole-log memo must miss.
	cur := transcriptPathFor(home, current)
	f, err := os.OpenFile(cur, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"user","message":{"content":"later-user"}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	os.Chtimes(cur, time.Now().Add(time.Second), time.Now().Add(time.Second))

	msgs, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 5 {
		t.Fatalf("got %d messages, want 5 (2 archived + 3 current): %+v", len(msgs), msgs)
	}
	if msgs[0].Text != "old-user" {
		t.Errorf("archived segment was re-read: msgs[0] = %q, want %q", msgs[0].Text, "old-user")
	}
	if msgs[4].Text != "later-user" {
		t.Errorf("current chain not refreshed: msgs[4] = %q, want %q", msgs[4].Text, "later-user")
	}
	for i := range msgs {
		if msgs[i].Index != i {
			t.Errorf("msg[%d].Index = %d, want %d", i, msgs[i].Index, i)
		}
	}
}

// TestDisplayMemo_HandsOutCopies guards the memo against its caller: serveHistory
// strips injected scaffolding in place, over a page that aliases this slice, so a
// memo sharing its backing array would serve the stripped text to everyone after.
func TestDisplayMemo_HandsOutCopies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	writeClaudeTranscript(t, home, id, "user-one", "asst-one")

	d := NewDriver()
	rec := &Session{Name: "s", SessionID: id}
	first, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	first[0].Text = "MUTATED"
	dropFileCaches(t)

	second, err := d.ReadDisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Text != "user-one" {
		t.Fatalf("caller's mutation reached the memo: got %q", second[0].Text)
	}
}

// TestDisplayHistory_MatchesDigest confirms the combined bodies+digest call agrees
// with computing the digest from the messages by hand — the invariant that lets
// serveHistory make one call instead of statting the chain twice.
func TestDisplayHistory_MatchesDigest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "11111111-1111-4111-8111-111111111111"
	writeClaudeTranscript(t, home, id, "user-one", "asst-one")

	d := NewDriver()
	rec := &Session{Name: "s", SessionID: id}
	msgs, count, hash, err := d.DisplayHistory(rec)
	if err != nil {
		t.Fatal(err)
	}
	wantCount, wantHash := HistoryDigest(msgs)
	if count != wantCount || hash != wantHash {
		t.Fatalf("DisplayHistory digest = (%d, %s), want (%d, %s)", count, hash, wantCount, wantHash)
	}
	dCount, dHash, _, err := d.DisplayDigest(rec)
	if err != nil {
		t.Fatal(err)
	}
	if dCount != count || dHash != hash {
		t.Fatalf("DisplayDigest = (%d, %s), want (%d, %s)", dCount, dHash, count, hash)
	}
}

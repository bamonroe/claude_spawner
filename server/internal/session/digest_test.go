package session

import "testing"

func msgs(texts ...string) []Message {
	out := make([]Message, len(texts))
	for i, t := range texts {
		out[i] = Message{Index: i, Role: "user", Text: t, Ts: int64(i)}
	}
	return out
}

func TestHistoryDigest_CountAndDeterminism(t *testing.T) {
	a := msgs("hello", "world", "again")
	n, h := HistoryDigest(a)
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
	if h == "" {
		t.Fatal("hash is empty")
	}
	// Same content → identical digest (the app relies on stable equality).
	n2, h2 := HistoryDigest(msgs("hello", "world", "again"))
	if n2 != n || h2 != h {
		t.Fatalf("non-deterministic digest: (%d,%s) vs (%d,%s)", n, h, n2, h2)
	}
}

func TestHistoryDigest_DetectsChange(t *testing.T) {
	_, base := HistoryDigest(msgs("a", "b"))

	// Appending a message changes the hash.
	if _, h := HistoryDigest(msgs("a", "b", "c")); h == base {
		t.Fatal("append did not change the hash")
	}
	// Editing text changes the hash (e.g. a clear/compress rewrite).
	if _, h := HistoryDigest(msgs("a", "B")); h == base {
		t.Fatal("edit did not change the hash")
	}
	// A role flip changes the hash even with identical text.
	flipped := msgs("a", "b")
	flipped[1].Role = "claude"
	if _, h := HistoryDigest(flipped); h == base {
		t.Fatal("role change did not change the hash")
	}
}

func TestHistoryDigest_Empty(t *testing.T) {
	n, h := HistoryDigest(nil)
	if n != 0 {
		t.Fatalf("count = %d, want 0", n)
	}
	if h == "" {
		t.Fatal("empty-chain hash should still be a stable non-empty digest")
	}
}

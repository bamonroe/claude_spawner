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

// idMsgs builds rows carrying durable ids, so the digest keys on ID not Index.
func idMsgs(ids ...string) []Message {
	out := make([]Message, len(ids))
	for i, id := range ids {
		out[i] = Message{Index: i, ID: id, Role: "user", Text: "t" + id, Ts: int64(i)}
	}
	return out
}

func TestHistoryDigest_IDKeyedSurvivesReindex(t *testing.T) {
	base := idMsgs("u1", "u2", "u3")
	_, h := HistoryDigest(base)

	// Same rows, same durable ids, but re-indexed (as a clear/compress rotation or
	// cross-backend concatenation would do): the digest must NOT change, so the app
	// skips a needless refetch.
	reindexed := idMsgs("u1", "u2", "u3")
	for i := range reindexed {
		reindexed[i].Index += 100
	}
	if _, h2 := HistoryDigest(reindexed); h2 != h {
		t.Fatalf("id-keyed digest changed on pure re-index: %s vs %s", h, h2)
	}

	// A genuine content change with the same ids still flips it.
	edited := idMsgs("u1", "u2", "u3")
	edited[1].Text = "changed"
	if _, h2 := HistoryDigest(edited); h2 == h {
		t.Fatal("id-keyed digest did not change on a text edit")
	}
	// A changed id (a different underlying row) flips it too.
	swapped := idMsgs("u1", "uX", "u3")
	if _, h2 := HistoryDigest(swapped); h2 == h {
		t.Fatal("id-keyed digest did not change when a row id changed")
	}
}

func TestHistoryDigest_IDlessStillIndexKeyed(t *testing.T) {
	// Rows without ids (Codex/Antigravity today) keep the positional behavior: a
	// re-index flips the hash, exactly as before this change.
	base := msgs("a", "b")
	_, h := HistoryDigest(base)
	shifted := msgs("a", "b")
	for i := range shifted {
		shifted[i].Index += 5
	}
	if _, h2 := HistoryDigest(shifted); h2 == h {
		t.Fatal("id-less digest should still change on a re-index (index-keyed fallback)")
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

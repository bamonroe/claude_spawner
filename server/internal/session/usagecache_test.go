package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestUsageCacheRoundTripsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	c := OpenUsageCache(path)
	c.Put("sess", "sig-1", &ContextSnapshot{Usage: Usage{Input: 7}, At: 99})

	got, ok := OpenUsageCache(path).Get("sess", "sig-1")
	if !ok || got == nil || got.Usage.Input != 7 || got.At != 99 {
		t.Fatalf("reopened cache Get = (%+v, %v), want the stored snapshot", got, ok)
	}
	if _, ok := c.Get("sess", "sig-2"); ok {
		t.Error("a changed signature must miss")
	}
	if _, ok := c.Get("other", "sig-1"); ok {
		t.Error("another session must miss")
	}
}

// A nil snapshot is a real answer worth caching: "no usage-bearing turn yet" is
// exactly the case that pays the full 32 MiB tail escalation for nothing.
func TestUsageCacheCachesAbsentSnapshot(t *testing.T) {
	c := OpenUsageCache("")
	c.Put("sess", "sig", nil)
	snap, ok := c.Get("sess", "sig")
	if !ok {
		t.Fatal("a cached nil snapshot must still be a hit")
	}
	if snap != nil {
		t.Fatalf("got %+v, want nil", snap)
	}
}

// An empty signature means "this chain can't describe itself cheaply" — there
// would be nothing to invalidate against, so it must never be stored or served.
func TestUsageCacheRejectsEmptySignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	c := OpenUsageCache(path)
	c.Put("sess", "", &ContextSnapshot{Usage: Usage{Input: 5}})
	if _, ok := c.Get("sess", ""); ok {
		t.Error("empty sig must never hit")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("empty sig must not write the cache file")
	}
}

// Callers get a copy: a snapshot handed out and then edited must not rewrite the
// cached entry (the gateway passes it straight into the attached frame).
func TestUsageCacheHandsOutCopies(t *testing.T) {
	c := OpenUsageCache("")
	c.Put("sess", "sig", &ContextSnapshot{Usage: Usage{Input: 3}})
	got, _ := c.Get("sess", "sig")
	got.Usage.Input = 999
	again, _ := c.Get("sess", "sig")
	if again.Usage.Input != 3 {
		t.Fatalf("caller's mutation reached the cache: got %d, want 3", again.Usage.Input)
	}
}

// A nil *UsageCache is the "no cache configured" wiring, which must be safe on:
// Driver.usage is nil until SetUsageCache is called.
func TestNilUsageCacheIsSafe(t *testing.T) {
	var c *UsageCache
	c.Put("sess", "sig", &ContextSnapshot{})
	if _, ok := c.Get("sess", "sig"); ok {
		t.Error("nil cache must never hit")
	}
}

// TestSessionContextUsage_ServesUnchangedChain confirms the attach-path lookup
// answers from the cache when the transcript hasn't moved — the whole point, since
// the `attached` ack waits on it.
func TestSessionContextUsage_ServesUnchangedChain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "22222222-2222-4222-8222-222222222222"
	proj := filepath.Join(home, ".claude", "projects", "-data")
	body := `{"type":"assistant","message":{"usage":{"input_tokens":11,"output_tokens":2}},"timestamp":"2024-01-01T00:00:00Z"}` + "\n"
	writeFile(t, filepath.Join(proj, id+".jsonl"), body)

	d := NewDriver()
	d.SetUsageCache(OpenUsageCache(""))
	rec := &Session{Name: "s", SessionID: id}
	first := d.SessionContextUsage(context.Background(), rec)
	if first == nil || first.Usage.Input != 11 {
		t.Fatalf("first read = %+v, want input 11", first)
	}
	// Same size and mtime, different bytes: only the cache can still say 11.
	same := `{"type":"assistant","message":{"usage":{"input_tokens":99,"output_tokens":2}},"timestamp":"2024-01-01T00:00:00Z"}` + "\n"
	rewriteSameStat(t, filepath.Join(proj, id+".jsonl"), same)
	dropFileCaches(t)

	if got := d.SessionContextUsage(context.Background(), rec); got == nil || got.Usage.Input != 11 {
		t.Fatalf("unchanged chain re-read from disk: %+v", got)
	}
}

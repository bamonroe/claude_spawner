package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestDigestCacheRoundTripsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digests.json")
	c := OpenDigestCache(path)
	c.Put("sess-1", "sig-a", 42, "hash-a")
	c.Sync()

	// Reopening is the case that matters: a restart must not lose the cache, which
	// is the whole reason it's on disk.
	reopened := OpenDigestCache(path)
	count, hash, ok := reopened.Get("sess-1", "sig-a")
	if !ok || count != 42 || hash != "hash-a" {
		t.Fatalf("Get after reopen = (%d, %q, %v), want (42, hash-a, true)", count, hash, ok)
	}
}

// A changed signature must miss. This is the invalidation the whole scheme rests
// on: a hit against a stale signature would serve the app a digest for content
// that no longer exists, and it would never refetch.
func TestDigestCacheMissesOnChangedSignature(t *testing.T) {
	c := OpenDigestCache("")
	c.Put("sess-1", "sig-a", 1, "hash-a")
	if _, _, ok := c.Get("sess-1", "sig-b"); ok {
		t.Fatal("a different signature must not hit")
	}
	if _, _, ok := c.Get("sess-2", "sig-a"); ok {
		t.Fatal("a different session must not hit")
	}
}

// An empty signature means "this chain can't be cheaply described" — it must
// never store or hit, or an unwatchable backend would get pinned forever.
func TestDigestCacheRefusesEmptySignature(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digests.json")
	c := OpenDigestCache(path)
	c.Put("sess-1", "", 7, "hash")
	if _, _, ok := c.Get("sess-1", ""); ok {
		t.Fatal("an empty signature must never hit")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("an empty-signature Put must not write the cache file")
	}
}

// A missing or corrupt file yields an empty cache rather than an error: losing
// the cache costs speed, never correctness.
func TestDigestCacheToleratesUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	if c := OpenDigestCache(filepath.Join(dir, "absent.json")); c == nil {
		t.Fatal("a missing file must still yield a usable cache")
	}
	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := OpenDigestCache(corrupt)
	if _, _, ok := c.Get("anything", "sig"); ok {
		t.Fatal("a corrupt file must read back as empty, not as a hit")
	}
	c.Put("sess", "sig", 1, "h") // must not panic, and must repair the file
	c.Sync()
	if _, _, ok := OpenDigestCache(corrupt).Get("sess", "sig"); !ok {
		t.Fatal("a Put after a corrupt read should leave a readable cache")
	}
}

// A nil cache is a supported configuration (caching disabled): every method has
// to be safe on it, because Driver.digests is nil until SetDigests is called.
func TestDigestCacheNilIsSafe(t *testing.T) {
	var c *DigestCache
	c.Put("k", "sig", 1, "h")
	if _, _, ok := c.Get("k", "sig"); ok {
		t.Fatal("a nil cache must never report a hit")
	}
}

// The signature has to change when a transcript's content changes. Size is the
// obvious signal; mtime catches a same-size rewrite.
func TestStatChainSigTracksSizeAndMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte("aaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	resolve := func(context.Context, string) string { return path }

	first, ok := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if !ok || first == "" {
		t.Fatal("expected a usable signature")
	}

	if err := os.WriteFile(path, []byte("aaaaaaaa"), 0o600); err != nil {
		t.Fatal(err)
	}
	grown, _ := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if grown == first {
		t.Fatal("a size change must change the signature")
	}

	// Same size, different mtime.
	if err := os.WriteFile(path, []byte("bbbbbbbb"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	touched, _ := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if touched == grown {
		t.Fatal("a same-size rewrite with a new mtime must change the signature")
	}
}

// An id with no transcript yet is a stable, legitimate state — but it must stop
// being stable the moment the file appears.
func TestStatChainSigInvalidatesWhenAnAbsentTranscriptAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "later.jsonl")
	resolve := func(context.Context, string) string {
		if _, err := os.Stat(path); err != nil {
			return ""
		}
		return path
	}
	absent, ok := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if !ok {
		t.Fatal("an absent transcript should still produce a signature")
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	present, _ := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if present == absent {
		t.Fatal("a transcript appearing must change the signature")
	}
}

// The sweep Puts from several goroutines at once. Every one of them has to
// survive to disk: marshalling under the lock but writing outside it let an
// earlier snapshot land after a later one, silently dropping sessions from the
// file (the first real sweep persisted 10 of 12 that way).
func TestDigestCacheConcurrentPutsAllPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digests.json")
	c := OpenDigestCache(path)

	const n = 24
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := fmt.Sprintf("sess-%02d", i)
			c.Put(key, "sig-"+key, i, "hash-"+key)
		}(i)
	}
	wg.Wait()
	c.Sync()

	reopened := OpenDigestCache(path)
	for i := range n {
		key := fmt.Sprintf("sess-%02d", i)
		count, hash, ok := reopened.Get(key, "sig-"+key)
		if !ok || count != i || hash != "hash-"+key {
			t.Fatalf("%s missing from the persisted cache: (%d, %q, %v)", key, count, hash, ok)
		}
	}
}

// Persistence is asynchronous and coalesced, so the thing that must hold is that
// the LAST value written for a key is the one on disk — a burst of rapid Puts
// must not leave an earlier snapshot as the survivor.
func TestDigestCacheCoalescedFlushKeepsTheNewestValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digests.json")
	c := OpenDigestCache(path)
	for i := range 50 {
		c.Put("sess", fmt.Sprintf("sig-%d", i), i, fmt.Sprintf("hash-%d", i))
	}
	c.Sync()

	count, hash, ok := OpenDigestCache(path).Get("sess", "sig-49")
	if !ok || count != 49 || hash != "hash-49" {
		t.Fatalf("persisted value = (%d, %q, %v), want the newest (49, hash-49, true)", count, hash, ok)
	}
}

// Put must not touch the disk: it edits the in-memory map and lets the store's
// debounced writer coalesce. A burst that used to be N whole-file rewrites
// (marshal+CreateTemp+rename per Put, under the lock) is now one, and no caller
// waits on it.
func TestDigestCachePutDoesNotWriteInline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "digests.json")
	c := OpenDigestCache(path)

	start := time.Now()
	for i := range 500 {
		key := fmt.Sprintf("sess-%03d", i)
		c.Put(key, "sig-"+key, i, "hash-"+key)
	}
	if elapsed := time.Since(start); elapsed > flushDebounce {
		t.Fatalf("500 Puts took %s — they are still writing inline", elapsed)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a Put wrote the cache file synchronously")
	}

	c.Sync() // ...and the coalesced write still persists all of them
	if _, _, ok := OpenDigestCache(path).Get("sess-499", "sig-sess-499"); !ok {
		t.Fatal("the debounced flush lost entries")
	}
}

// A resolver that returns a deterministic path for a file that doesn't exist
// (antigravity computes its brain-transcript path rather than globbing for it)
// must still yield a usable signature. Treating the failed stat as "give up"
// made such a session permanently uncacheable — it re-parsed its whole chain on
// every sweep, and one archived antigravity segment did exactly that in
// production.
func TestStatChainSigHandlesADeterministicPathThatDoesNotExist(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "never-created.jsonl")
	resolve := func(context.Context, string) string { return missing } // deterministic, not a glob

	sig, ok := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"})
	if !ok {
		t.Fatal("a deterministic path to a missing file must still be cacheable")
	}
	if sig == "" {
		t.Fatal("expected a non-empty signature")
	}
	// And it must stop matching once the file shows up.
	if err := os.WriteFile(missing, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if now, _ := statChainSig(context.Background(), localClaudeFS, resolve, []string{"id"}); now == sig {
		t.Fatal("the signature must change once the transcript appears")
	}
}

// A digest persisted by an older PARSER must not be served for a transcript whose
// bytes haven't changed since. This is the codex history-loss bug: the pre-
// item_completed codex parser read every current rollout as zero rows and the
// durable cache pinned that empty digest to the rollout's (unchanged) stat, so
// even a fixed server kept telling the app an idle codex session was empty.
func TestDisplayDigestIgnoresDigestFromAnOlderParser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	id := "01a16933-ad72-7ff3-a484-f00c116c8457"
	dir := filepath.Join(home, ".codex", "sessions", "2026", "10", "23")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","content":[{"text":"hi"}]}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"msg_1","content":[{"text":"hello"}]}}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-23T10-39-17-"+id+".jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDriver()
	d.SigTTL(0)
	digests := OpenDigestCache("")
	d.SetDigests(digests)
	rec := &Session{Name: "apple", Agent: "codex", SessionID: id}

	// Seed what the old binary persisted: an empty digest under the bare, unstamped
	// signature of these very bytes.
	bare, ok := codexFS{}.chainSig(context.Background(), []string{id})
	if !ok {
		t.Fatal("expected a usable signature")
	}
	emptyCount, emptyHash := HistoryDigest(nil)
	digests.Put(id, "codex@"+bare, emptyCount, emptyHash)

	count, _, cached, err := d.DisplayDigest(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if cached || count != 2 {
		t.Fatalf("DisplayDigest = (count %d, cached %v), want a recomputed 2 rows", count, cached)
	}
}

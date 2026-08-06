package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// UsageCache remembers each session's last context snapshot (the token sizes of
// its most recent usage-bearing turn) across restarts, keyed by a cheap freshness
// signature of the transcripts it was read from.
//
// It exists because attaching to a session reads that snapshot BEFORE the
// `attached` frame is written, and the read is a backward tail scan of the
// transcript that escalates 256 KiB → 4 MiB → 32 MiB until it finds a
// usage-bearing assistant line. Locally that's milliseconds; over SSH it's a
// `tail -c` of up to 32 MiB on a channel that sustains about a megabyte a second,
// sitting directly in front of the ack the client waits on. The in-memory snapshot
// cache can't help: a turn changes the transcript's size and mtime, so the very
// next attach — the common one, right after the session produced output — always
// misses, and after a restart every session misses.
//
// The signature is the same (path + size + mtime, per transcript in the chain)
// freshness proxy DigestCache uses, so a stat answers what a multi-megabyte read
// used to. A nil snapshot is cached too: a fresh session with no usage anywhere is
// exactly the case that pays the full 32 MiB escalation for nothing.
//
// Safe for concurrent use. Persistence is best-effort: a cache that can't be read
// or written costs speed, never correctness.
type UsageCache struct {
	path string

	mu      sync.Mutex
	entries map[string]usageEntry // session_id → last snapshot + the signature it was read at
}

type usageEntry struct {
	Sig string `json:"sig"`
	// Usage is nil when the chain genuinely has no usage-bearing turn yet.
	Usage *ContextSnapshot `json:"usage"`
}

// OpenUsageCache loads the cache at path, or returns an empty one if it's missing
// or unreadable. path may be "" for a memory-only cache (tests).
func OpenUsageCache(path string) *UsageCache {
	c := &UsageCache{path: path, entries: map[string]usageEntry{}}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var loaded map[string]usageEntry
	if json.Unmarshal(data, &loaded) == nil && loaded != nil {
		c.entries = loaded
	}
	return c
}

// Get returns the cached snapshot for key when it was read at exactly this
// signature. An empty sig is never a hit — that's the "can't cheaply describe this
// chain" case, which must always re-read. A hit with a nil snapshot is still a
// hit: "this chain has no usage" is worth caching.
func (c *UsageCache) Get(key, sig string) (*ContextSnapshot, bool) {
	if c == nil || sig == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found || e.Sig != sig {
		return nil, false
	}
	if e.Usage == nil {
		return nil, true
	}
	snap := *e.Usage // copy: callers must not be able to edit the cache in place
	return &snap, true
}

// Put records a snapshot against the signature it was read at and persists the
// cache. An empty sig is dropped: with nothing to invalidate against, a stale
// snapshot would leave the app showing the wrong context size indefinitely.
func (c *UsageCache) Put(key, sig string, snap *ContextSnapshot) {
	if c == nil || sig == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if found && e.Sig == sig && sameSnapshot(e.Usage, snap) {
		return // unchanged; no need to rewrite the file
	}
	stored := snap
	if snap != nil {
		v := *snap
		stored = &v
	}
	c.entries[key] = usageEntry{Sig: sig, Usage: stored}
	data, err := json.Marshal(c.entries)
	if err != nil || c.path == "" {
		return
	}
	// Marshal AND write under the lock, for the reason DigestCache.Put documents:
	// concurrent Puts writing outside the lock let an earlier snapshot of the map
	// land after a later one, silently dropping sessions from the file.
	c.write(data)
}

func sameSnapshot(a, b *ContextSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// write replaces the cache file atomically, so a crash mid-write can't leave a
// truncated file that reads back as an empty cache.
func (c *UsageCache) write(data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".usage-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return
	}
	if err := os.Rename(name, c.path); err != nil {
		os.Remove(name)
	}
}

package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// DigestCache remembers each session's transcript digest (message count +
// content hash) across restarts, keyed by a cheap freshness signature of the
// transcripts it was computed from.
//
// It exists because computing a digest requires PARSING a session's whole
// transcript chain, and the app validates its offline cache by asking for every
// session's digest on connect. In-memory memoization made that cheap only until
// the next restart; after one, the sweep re-read every session from scratch —
// ~105 s on a real registry, every time the container was recreated. A stat is
// several orders of magnitude cheaper than a parse, so when nothing has changed
// the sweep does a handful of stats and no reads at all.
//
// The signature is the same freshness proxy the in-memory transcript cache
// already trusts (path + size + mtime, per transcript in the chain). A file
// whose size and mtime both match is treated as unchanged; anything else
// recomputes.
//
// Safe for concurrent use. Persistence is best-effort: a cache that can't be
// read or written costs speed, never correctness, so errors are swallowed and
// the caller just recomputes.
type DigestCache struct {
	path string

	mu      sync.Mutex
	entries map[string]digestEntry // session_id → last digest + the signature it was computed from
	dirty   bool
}

type digestEntry struct {
	Sig   string `json:"sig"`
	Count int    `json:"count"`
	Hash  string `json:"hash"`
}

// OpenDigestCache loads the cache at path, or returns an empty one if it's
// missing or unreadable. path may be "" for a memory-only cache (tests).
func OpenDigestCache(path string) *DigestCache {
	c := &DigestCache{path: path, entries: map[string]digestEntry{}}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var loaded map[string]digestEntry
	if json.Unmarshal(data, &loaded) == nil && loaded != nil {
		c.entries = loaded
	}
	return c
}

// Get returns the cached digest for key when it was computed from exactly this
// signature. An empty sig is never a hit — that's the "can't cheaply describe
// this chain" case, which must always recompute.
func (c *DigestCache) Get(key, sig string) (count int, hash string, ok bool) {
	if c == nil || sig == "" {
		return 0, "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if !found || e.Sig != sig {
		return 0, "", false
	}
	return e.Count, e.Hash, true
}

// Put records a digest against the signature it was computed from and persists
// the cache. An empty sig is dropped: without a freshness signature there is
// nothing to invalidate against, and a stale digest would make the app show a
// stale transcript.
func (c *DigestCache) Put(key, sig string, count int, hash string) {
	if c == nil || sig == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, found := c.entries[key]
	if found && e.Sig == sig && e.Count == count && e.Hash == hash {
		return // unchanged; no need to rewrite the file
	}
	c.entries[key] = digestEntry{Sig: sig, Count: count, Hash: hash}
	data, err := json.Marshal(c.entries)
	if err != nil || c.path == "" {
		return
	}
	// Write while still holding the lock. The sweep Puts from several goroutines
	// at once, and marshalling under the lock but writing outside it lets an
	// earlier snapshot land after a later one — a lost update that silently drops
	// sessions from the file. (It did: the first real sweep persisted 10 of 12.)
	// The file is a few tens of KB and sweeps are rare, so serializing the write
	// costs nothing worth measuring.
	c.write(data)
}

// write replaces the cache file atomically, so a crash mid-write can't leave a
// truncated file that reads back as an empty cache.
func (c *DigestCache) write(data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(c.path), ".digests-*")
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

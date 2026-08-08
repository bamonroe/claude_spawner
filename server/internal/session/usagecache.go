package session

// (persistence lives in jsonStore — see jsonstore.go)

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
// Safe for concurrent use. Persistence is best-effort and asynchronous (see
// jsonStore): a cache that can't be read or written costs speed, never
// correctness.
type UsageCache struct {
	// session_id → last snapshot + the signature it was read at
	store *jsonStore[usageEntry]
}

type usageEntry struct {
	Sig string `json:"sig"`
	// Usage is nil when the chain genuinely has no usage-bearing turn yet.
	Usage *ContextSnapshot `json:"usage"`
}

// OpenUsageCache loads the cache at path, or returns an empty one if it's missing
// or unreadable. path may be "" for a memory-only cache (tests).
func OpenUsageCache(path string) *UsageCache {
	return &UsageCache{store: openJSONStore[usageEntry](path, ".usage-*")}
}

// Sync blocks until every snapshot recorded so far is on disk. Tests use it;
// nothing on a request path should.
func (c *UsageCache) Sync() {
	if c == nil {
		return
	}
	c.store.sync()
}

// Get returns the cached snapshot for key when it was read at exactly this
// signature. An empty sig is never a hit — that's the "can't cheaply describe this
// chain" case, which must always re-read. A hit with a nil snapshot is still a
// hit: "this chain has no usage" is worth caching.
func (c *UsageCache) Get(key, sig string) (*ContextSnapshot, bool) {
	if c == nil || sig == "" {
		return nil, false
	}
	e, found := c.store.get(key)
	if !found || e.Sig != sig {
		return nil, false
	}
	if e.Usage == nil {
		return nil, true
	}
	snap := *e.Usage // copy: callers must not be able to edit the cache in place
	return &snap, true
}

// Last returns the last snapshot recorded for key at ANY signature — the newest
// value we know without touching the host. It is deliberately unvalidated: callers
// use it as a provisional badge on a latency-critical path (attach) and follow up
// with the authoritative SessionContextUsage read off that path. Never use it
// where the answer must be current.
func (c *UsageCache) Last(key string) *ContextSnapshot {
	if c == nil {
		return nil
	}
	e, found := c.store.get(key)
	if !found || e.Usage == nil {
		return nil
	}
	snap := *e.Usage // copy: callers must not be able to edit the cache in place
	return &snap
}

// Put records a snapshot against the signature it was read at; the store
// persists it asynchronously. An empty sig is dropped: with nothing to
// invalidate against, a stale snapshot would leave the app showing the wrong
// context size indefinitely.
func (c *UsageCache) Put(key, sig string, snap *ContextSnapshot) {
	if c == nil || sig == "" {
		return
	}
	c.store.update(key, func(e usageEntry, found bool) (usageEntry, bool) {
		if found && e.Sig == sig && sameSnapshot(e.Usage, snap) {
			return e, false // unchanged; nothing to persist
		}
		stored := snap
		if snap != nil {
			v := *snap // copy: the caller must not be able to edit the cache in place
			stored = &v
		}
		return usageEntry{Sig: sig, Usage: stored}, true
	})
}

func sameSnapshot(a, b *ContextSnapshot) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

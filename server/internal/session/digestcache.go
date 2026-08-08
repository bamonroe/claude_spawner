package session

// (persistence lives in jsonStore — see jsonstore.go)

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
// Safe for concurrent use. Persistence is best-effort and asynchronous (see
// jsonStore): a cache that can't be read or written costs speed, never
// correctness, so errors are swallowed and the caller just recomputes.
type DigestCache struct {
	// session_id → last digest + the signature it was computed from
	store *jsonStore[digestEntry]
}

type digestEntry struct {
	Sig   string `json:"sig"`
	Count int    `json:"count"`
	Hash  string `json:"hash"`
}

// OpenDigestCache loads the cache at path, or returns an empty one if it's
// missing or unreadable. path may be "" for a memory-only cache (tests).
func OpenDigestCache(path string) *DigestCache {
	return &DigestCache{store: openJSONStore[digestEntry](path, ".digests-*")}
}

// Sync blocks until every digest recorded so far is on disk. Tests use it;
// nothing on a request path should.
func (c *DigestCache) Sync() {
	if c == nil {
		return
	}
	c.store.sync()
}

// Get returns the cached digest for key when it was computed from exactly this
// signature. An empty sig is never a hit — that's the "can't cheaply describe
// this chain" case, which must always recompute.
func (c *DigestCache) Get(key, sig string) (count int, hash string, ok bool) {
	if c == nil || sig == "" {
		return 0, "", false
	}
	e, found := c.store.get(key)
	if !found || e.Sig != sig {
		return 0, "", false
	}
	return e.Count, e.Hash, true
}

// Put records a digest against the signature it was computed from; the store
// persists it asynchronously. An empty sig is dropped: without a freshness
// signature there is nothing to invalidate against, and a stale digest would
// make the app show a stale transcript.
func (c *DigestCache) Put(key, sig string, count int, hash string) {
	if c == nil || sig == "" {
		return
	}
	c.store.update(key, func(e digestEntry, found bool) (digestEntry, bool) {
		if found && e.Sig == sig && e.Count == count && e.Hash == hash {
			return e, false // unchanged; nothing to persist
		}
		return digestEntry{Sig: sig, Count: count, Hash: hash}, true
	})
}

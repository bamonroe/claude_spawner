package session

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// PurgeQueue is the durable record of remote transcript purges a delete still
// OWES, when the machine holding those files was unreachable at delete time.
//
// It exists because deleting a session used to be hostage to its host being up.
// The purge walks a session's whole transcript chain over SSH (several commands
// per id, per backend segment), each bounded only by a 30 s probe timeout — so
// deleting a session on a sleeping laptop appeared to hang, and the one session
// the user most wanted gone was the one they couldn't remove. Splitting the two
// halves fixes that at the seam: the registry record goes NOW (the local half is
// never blocked by a remote one), and the remote half becomes an owed item here,
// retried whenever that host comes back. Nothing is silently leaked — the debt is
// on disk and survives a restart.
//
// Safe for concurrent use. Persistence is best-effort in the same sense as the
// other caches: an unwritable file costs durability across a restart, never
// correctness of the running server.
type PurgeQueue struct {
	path string

	mu    sync.Mutex
	items []PurgeItem
}

// maxPurgeAttempts bounds how many times a single owed purge is retried before
// the debt is abandoned (logged, then dropped). At the 6-minute retry tick that
// is roughly an hour of trying. The cap exists because a retry is not free: it
// runs a real command on the host, and an item whose command can never succeed
// would otherwise re-run it on the ticker forever, for the rest of the server's
// life, with nothing in the log to say so.
const maxPurgeAttempts = 10

// PurgeItem is one owed purge: a backend's transcript ids on one host. A single
// deleted session can owe several (a chain that switched backends or hosts keeps
// one item per segment).
type PurgeItem struct {
	Session  string    `json:"session"` // the deleted session's name, for logging only
	Agent    string    `json:"agent"`   // backend id, selects the on-disk format
	Host     string    `json:"host"`    // "" = local
	IDs      []string  `json:"ids"`
	Created  time.Time `json:"created"`
	Attempts int       `json:"attempts"`
}

// key identifies an item for dedup: the same (agent, host, ids) owed twice is one
// debt. Delete is idempotent, but re-queuing would grow the file without bound if
// a caller retried a delete.
func (it PurgeItem) key() string {
	k := it.Agent + "\x00" + it.Host
	for _, id := range it.IDs {
		k += "\x00" + id
	}
	return k
}

// OpenPurgeQueue loads the queue at path, or returns an empty one if it's missing
// or unreadable. path may be "" for a memory-only queue (tests).
func OpenPurgeQueue(path string) *PurgeQueue {
	q := &PurgeQueue{path: path}
	if path == "" {
		return q
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return q
	}
	var loaded []PurgeItem
	if json.Unmarshal(data, &loaded) == nil {
		q.items = loaded
	}
	return q
}

// Add records owed purges, skipping empties and duplicates, and persists.
func (q *PurgeQueue) Add(items ...PurgeItem) {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	have := map[string]bool{}
	for _, it := range q.items {
		have[it.key()] = true
	}
	added := false
	for _, it := range items {
		if len(it.IDs) == 0 || have[it.key()] {
			continue
		}
		have[it.key()] = true
		q.items = append(q.items, it)
		added = true
	}
	if added {
		q.flushLocked()
	}
}

// Pending returns a copy of the owed items (empty when the queue is nil).
func (q *PurgeQueue) Pending() []PurgeItem {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]PurgeItem, len(q.items))
	copy(out, q.items)
	return out
}

// Resolve retries every owed item through done and drops the ones it settles.
// done reports true when the debt is discharged (purged, or provably gone);
// false leaves the item queued with its attempt count bumped, so a host that is
// still down simply keeps its debt — up to maxPurgeAttempts, past which the item
// is logged and dropped rather than retried forever. Returns how many items were
// cleared (settled ones only; an abandoned item is not "cleared").
func (q *PurgeQueue) Resolve(done func(PurgeItem) bool) int {
	if q == nil {
		return 0
	}
	pending := q.Pending()
	if len(pending) == 0 {
		return 0
	}
	settled := map[string]bool{}
	for _, it := range pending {
		if done(it) {
			settled[it.key()] = true
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	kept := q.items[:0]
	for _, it := range q.items {
		if settled[it.key()] {
			continue
		}
		it.Attempts++
		// Give up on a debt that has failed too many times, loudly. See
		// maxPurgeAttempts: an item retried forever is a command re-run on the host
		// on every tick with no prospect of succeeding.
		if it.Attempts >= maxPurgeAttempts {
			log.Printf("deferred purge for deleted session[%s] on host %q abandoned after %d attempts; %d transcript id(s) left on disk",
				it.Session, it.Host, it.Attempts, len(it.IDs))
			continue
		}
		kept = append(kept, it)
	}
	q.items = kept
	q.flushLocked()
	return len(settled)
}

// flushLocked writes the queue atomically. Caller holds q.mu — the write stays
// under the lock so two concurrent mutations can't land out of order and lose a
// debt (the same reason DigestCache serializes its writes).
func (q *PurgeQueue) flushLocked() {
	if q.path == "" {
		return
	}
	data, err := json.Marshal(q.items)
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(q.path), ".purges-*")
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
	if err := os.Rename(name, q.path); err != nil {
		os.Remove(name)
	}
}

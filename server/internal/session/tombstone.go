package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrStale is returned by an app-managed catalogue store (hosts, identities,
// profiles) when an incoming upsert or delete carries an `updated_at` older than
// what the store already holds — the last-writer-wins arbitration rejects it. The
// gateway treats this not as a failure but as a signal to re-send its newer record
// to the stale client so the client adopts it. Real errors (bad input, I/O) are
// returned as themselves; only a genuinely-stale write is ErrStale.
var ErrStale = errors.New("stale write rejected (older updated_at)")

// tombstones is a per-catalogue record of deleted keys and the millisecond
// `updated_at` at which each was removed. It is consulted during last-writer-wins
// arbitration: a re-add whose stamp is not newer than the tombstone is ignored (a
// stale client cannot resurrect a removed record), while a strictly-newer add
// resurrects the key and clears its tombstone. Persisted as a JSON sidecar next to
// the catalogue's own file (path + ".tomb") so it survives restarts without
// changing the catalogue file's existing on-disk shape.
type tombstones map[string]int64

// tombPath is the sidecar file holding a catalogue's tombstones.
func tombPath(cataloguePath string) string {
	if cataloguePath == "" {
		return ""
	}
	return cataloguePath + ".tomb"
}

// loadTombstones reads the tombstone sidecar for a catalogue file. A missing file
// (the common case — no delete has ever happened) is an empty set.
func loadTombstones(cataloguePath string) (tombstones, error) {
	m := tombstones{}
	p := tombPath(cataloguePath)
	if p == "" {
		return m, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse tombstones %s: %w", p, err)
	}
	return m, nil
}

// flushTombstones writes the tombstone sidecar atomically (temp file + rename). An
// in-memory store (empty path) persists nothing.
func flushTombstones(cataloguePath string, m tombstones) error {
	p := tombPath(cataloguePath)
	if p == "" {
		return nil
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(p); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// tombstone records key's removal at updatedAt, keeping the newest stamp on repeat.
func (m tombstones) tombstone(key string, updatedAt int64) {
	if updatedAt > m[key] {
		m[key] = updatedAt
	}
}

// blocksAdd reports whether a re-add of key stamped at updatedAt must be rejected:
// a tombstone at or newer than the add wins (the delete is at least as recent).
func (m tombstones) blocksAdd(key string, updatedAt int64) bool {
	ts, ok := m[key]
	return ok && updatedAt <= ts
}

// clearIfOlder removes key's tombstone when a strictly-newer add (updatedAt) has
// resurrected the record, so a tombstone never lingers past a live record.
func (m tombstones) clearIfOlder(key string, updatedAt int64) {
	if ts, ok := m[key]; ok && updatedAt > ts {
		delete(m, key)
	}
}

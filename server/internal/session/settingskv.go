package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// SettingRecord is one genuinely-shared server-global scalar setting, modeled as a
// keyed record so the versioned sync layer (last-writer-wins + tombstones + the
// per-catalogue digest) arbitrates each key independently — exactly like the host,
// identity, profile, and provider catalogues. Value is ALWAYS a string on the wire
// (booleans as "true"/"false", ints as decimal); it is typed only at the
// consuming/UI edges. The keys are the six routed through this store:
//
//	whisper_model, whisper_fast_model  — resident whisper server model names
//	warm_compress, auto_compress       — auto-compress triggers ("true"/"false")
//	auto_compress_threshold            — context-token limit, in thousands (decimal)
//	summary_only                       — speak-only-the-final-result mode ("true"/"false")
type SettingRecord struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// UpdatedAt is the client-stamped last-edit time in unix MILLISECONDS driving
	// last-writer-wins arbitration; a server-originated change (e.g. a whisper load
	// completing) stamps wall-clock ms too. 0 = a pre-timestamp record.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// SettingKV is a concurrency-safe, file-backed keyed store of [SettingRecord]s: the
// fifth app-managed catalogue (settings). It clones the shape of the other catalogue
// stores (HostStore in particular) — atomic JSON persistence, per-key last-writer-wins
// with a tombstone sidecar for parity — so a setting flows through the identical
// versioned sync machinery as hosts/identities/profiles/providers. A setting is
// unlikely to ever be deleted, but the tombstone path is kept for parity.
type SettingKV struct {
	path  string
	mu    sync.RWMutex
	byKey map[string]*SettingRecord
	tombs tombstones
}

// OpenSettingKV loads (or initializes) the keyed settings store at path. An empty
// path yields an in-memory-only store (flush is a no-op) so the server still runs
// when SPAWNER_STATE is unset. A missing file is a clean first run (no records).
func OpenSettingKV(path string) (*SettingKV, error) {
	s := &SettingKV{path: path, byKey: map[string]*SettingRecord{}}
	tombs, err := loadTombstones(path)
	if err != nil {
		return nil, err
	}
	s.tombs = tombs
	if path == "" {
		return s, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // fresh store
		}
		return nil, err
	}
	var list []*SettingRecord
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	for _, r := range list {
		if r != nil && r.Key != "" {
			s.byKey[r.Key] = r
		}
	}
	return s, nil
}

// List returns all records, sorted by key (stable order for iteration; the digest
// is order-independent regardless).
func (s *SettingKV) List() []*SettingRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*SettingRecord, 0, len(s.byKey))
	for _, r := range s.byKey {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Get returns the record for a key, or nil.
func (s *SettingKV) Get(key string) *SettingRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byKey[key]
}

// Value returns the string value stored for key, or "" when absent — the caller
// types it (bool/int) at the consuming edge and falls back to its own default.
func (s *SettingKV) Value(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r := s.byKey[key]; r != nil {
		return r.Value
	}
	return ""
}

// Put upserts a record by key and persists, arbitrating by last-writer-wins: an
// incoming record whose UpdatedAt is older than the stored one — or not newer than
// a tombstone from a more-recent delete — is rejected with ErrStale (the gateway then
// re-broadcasts the newer record). A strictly-newer add clears any tombstone. On an
// equal stamp the incoming value wins (idempotent re-broadcasts apply cleanly).
func (s *SettingKV) Put(rec *SettingRecord) error {
	if rec == nil || rec.Key == "" {
		return fmt.Errorf("setting needs a key")
	}
	s.mu.Lock()
	if s.tombs == nil {
		s.tombs = tombstones{}
	}
	if cur := s.byKey[rec.Key]; cur != nil {
		if rec.UpdatedAt < cur.UpdatedAt {
			s.mu.Unlock()
			return ErrStale
		}
	} else if s.tombs.blocksAdd(rec.Key, rec.UpdatedAt) {
		s.mu.Unlock()
		return ErrStale
	}
	s.tombs.clearIfOlder(rec.Key, rec.UpdatedAt)
	cp := *rec
	s.byKey[rec.Key] = &cp
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a key and records a tombstone at updatedAt so a stale client cannot
// resurrect it with an older stamp. Kept for parity with the other catalogues; a
// delete older than the stored record is rejected with ErrStale.
func (s *SettingKV) Delete(key string, updatedAt int64) error {
	s.mu.Lock()
	if s.tombs == nil {
		s.tombs = tombstones{}
	}
	if cur := s.byKey[key]; cur != nil && updatedAt < cur.UpdatedAt {
		s.mu.Unlock()
		return ErrStale
	}
	delete(s.byKey, key)
	s.tombs.tombstone(key, updatedAt)
	s.mu.Unlock()
	return s.flush()
}

// flush writes the store atomically (temp file + rename). No-op with no path.
func (s *SettingKV) flush() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	list := make([]*SettingRecord, 0, len(s.byKey))
	for _, r := range s.byKey {
		list = append(list, r)
	}
	tombs := make(tombstones, len(s.tombs))
	for k, v := range s.tombs {
		tombs[k] = v
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	return flushTombstones(s.path, tombs)
}

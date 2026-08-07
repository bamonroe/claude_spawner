package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Store is a concurrency-safe, file-backed registry of Session records. Because
// sessions are durable (a session_id on disk, not a process), the registry
// survives server restarts: on boot we can list known sessions and reattach.
type Store struct {
	path string
	mu   sync.RWMutex
	// Records are indexed both by their mutable Name (the voice/lookup handle) and
	// by their stable SessionID (the durable identity). A rename only re-keys
	// byName; byID never moves — so callers that hold a session_id (attach/rename/
	// delete, the job hub) resolve it in O(1) and unambiguously.
	byName map[string]*Session
	byID   map[string]*Session
}

// OpenStore loads (or initializes) the registry at path.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, byName: map[string]*Session{}, byID: map[string]*Session{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // fresh store
		}
		return nil, err
	}
	var list []*Session
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse store %s: %w", path, err)
	}
	for _, rec := range list {
		// Migrate records written before the host became explicit: a host-target
		// session with no Host used to mean "loopback". Name it LocalHost so nothing
		// relies on the old implicit default (the SSH executor now rejects a hostless
		// host-target session). Sandbox sessions keep their empty Host — the sandbox
		// path ignores it.
		if rec.Host == "" && rec.Target != TargetSandbox {
			rec.Host = LocalHost
		}
		s.byName[rec.Name] = rec
		if rec.SessionID != "" {
			s.byID[rec.SessionID] = rec
		}
	}
	// Self-heal phantom duplicates: a folder may hold any number of distinct
	// sessions (each its own session_id), but two records that share the SAME
	// session_id are the same underlying conversation split in two — e.g. a rename
	// or adopt that wrote a second row. Collapse those on load, keeping the primary
	// and dropping the rest, so the list self-cleans on the next restart. Records
	// with different session_ids in one dir are legitimate and left alone.
	if n := s.dedupeBySessionID(); n > 0 {
		if err := s.flush(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// dedupeBySessionID collapses records that share the same non-empty session_id
// down to a single primary, dropping the others from both indices. Returns how
// many records it removed. Two rows with one session_id are the same underlying
// --resume conversation recorded twice (a stale adopt/rename); distinct
// session_ids — even in the same directory — are separate sessions and are kept.
// The primary is the most "real" record: a Started session beats a not-started
// one, an explicit host-target beats an empty target (registerDiscovered leaves
// Target empty; a spawned/typed session sets it), and ties break on the
// lexicographically-first name (the base "<dir>", not the deduped "<dir>-2").
// Records with no session_id can't be resume-duplicates and are left alone.
// Caller holds no lock (invoked from OpenStore before the store is shared) and is
// responsible for flushing.
func (s *Store) dedupeBySessionID() (removed int) {
	byID := map[string][]*Session{}
	for _, rec := range s.byName {
		if rec.SessionID == "" {
			continue // no durable id — can't be a resume-duplicate
		}
		byID[rec.SessionID] = append(byID[rec.SessionID], rec)
	}
	for id, recs := range byID {
		if len(recs) < 2 {
			continue
		}
		primary := recs[0]
		for _, rec := range recs[1:] {
			if localPrimacy(rec, primary) {
				primary = rec
			}
		}
		for _, rec := range recs {
			if rec == primary {
				continue
			}
			delete(s.byName, rec.Name)
			removed++
		}
		s.byID[id] = primary // the index may have pointed at a dropped record
	}
	return removed
}

// localPrimacy reports whether a should win over b as a folder's primary record.
func localPrimacy(a, b *Session) bool {
	if a.Started != b.Started {
		return a.Started // a started session outranks a not-started one
	}
	aHost, bHost := a.Target == TargetHost, b.Target == TargetHost
	if aHost != bHost {
		return aHost // an explicit host-target outranks an empty one
	}
	return a.Name < b.Name // stable tiebreak: the base name beats "<dir>-2"
}

// Get returns the session by name, or nil.
func (s *Store) Get(name string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[name]
}

// GetByDir returns the registered session for a directory, or nil. If several
// records share a directory, the lexicographically-first by name is returned
// (matching the old List()-and-break callers).
func (s *Store) GetByDir(dir string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best, bestSnap *Session
	for _, rec := range s.byName {
		// Read the record's fields under ITS lock — the map lock says nothing about
		// them, and Dir/Name can be rewritten by a rename while this scan runs.
		snap := rec.Snapshot()
		if snap.Dir == dir && (best == nil || snap.Name < bestSnap.Name) {
			best, bestSnap = rec, snap
		}
	}
	return best
}

// GetByDirHost returns the registered session at dir that runs in a specific
// execution location — an SSH host (host non-empty, host-target sessions only) or
// the local sandbox (host empty, sandbox sessions only). This is what the spawn
// picker dedups against: a folder may legitimately host one session per host, so
// matching by directory alone would wrongly reuse (say) the localhost session when
// the user asked for a remote one. nil if none; ties broken by name.
func (s *Store) GetByDirHost(dir, host string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var best, bestSnap *Session
	for _, rec := range s.byName {
		snap := rec.Snapshot() // fields under the record's own lock, not the map lock
		if snap.Dir != dir {
			continue
		}
		match := snap.Host == host && snap.Target != TargetSandbox
		if host == "" {
			match = snap.Target == TargetSandbox
		}
		if match && (best == nil || snap.Name < bestSnap.Name) {
			best, bestSnap = rec, snap
		}
	}
	return best
}

// GetBySessionID returns the registered session with the given session_id, or
// nil. session_ids are globally unique, so at most one record matches.
func (s *Store) GetBySessionID(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byID[id]
}

// GetByAnyID resolves a session by any transcript id it has ever run under: its
// live SessionID, an id retired via a "clear"/"compress" context rotation
// (PriorIDs), or an id archived by a backend switch (History). A caller holding
// a pre-rotation id — e.g. an app attaching by an id that rotated while it was
// away, or a "previous session" pointer captured before a clear — still finds
// the live record. Falls back to an OwnsID scan only when the fast byID lookup
// misses.
func (s *Store) GetByAnyID(id string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if rec := s.byID[id]; rec != nil {
		return rec
	}
	for _, rec := range s.byName {
		if rec.OwnsID(id) {
			return rec
		}
	}
	return nil
}

// List returns all sessions sorted by name.
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.byName))
	names := map[*Session]string{}
	for _, rec := range s.byName {
		out = append(out, rec)
		rec.Read(func(r *Session) { names[rec] = r.Name }) // Name races a rename
	}
	sort.Slice(out, func(i, j int) bool { return names[out[i]] < names[out[j]] })
	return out
}

// Insert registers a NEW record: it resolves rec's Name to one no existing
// record holds (base, then "base-2", "base-3", …) and inserts into both indices
// under ONE lock. This replaces the old resolve-then-Put pair, which was a
// TOCTOU race: two concurrent adoptions/spawns in the same dir both computed
// "spawner" and the second Put silently overwrote the first in byName — the
// record vanished from List() while staying live in byID.
//
// Insert is also idempotent on the session_id, under the same lock: if any
// registered record already owns rec's id (live, retired by clear/compress, or
// archived by a backend switch), that record is returned and rec is NOT
// inserted — two concurrent adoptions of one id can't mint duplicate records.
// The returned *Session is therefore the record that owns the id after the
// call: rec itself on a fresh insert, the existing owner otherwise.
func (s *Store) Insert(rec *Session) (*Session, error) {
	var base, id string
	rec.Read(func(r *Session) { base, id = r.Name, r.SessionID })
	s.mu.Lock()
	if id != "" {
		if existing := s.byID[id]; existing != nil {
			s.mu.Unlock()
			return existing, nil
		}
		for _, r := range s.byName {
			if r.OwnsID(id) { // store→record lock order, same as List/GetByAnyID
				s.mu.Unlock()
				return r, nil
			}
		}
	}
	name := base
	for i := 2; ; i++ {
		if _, taken := s.byName[name]; !taken {
			break
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	if name != base {
		rec.Mutate(func(r *Session) { r.Name = name })
	}
	s.byName[name] = rec
	if id != "" {
		s.byID[id] = rec
	}
	s.mu.Unlock()
	return rec, s.flush()
}

// Put updates a session (use Insert to register a new one — Put does no name
// allocation) and persists the registry. Any stale index key still pointing at
// this record — an old name, or a session_id rotated away by clear/compress —
// is purged in the same locked pass, so an index entry can never shadow or leak
// a record whose keys have moved on.
func (s *Store) Put(rec *Session) error {
	var name, id string
	rec.Read(func(r *Session) { name, id = r.Name, r.SessionID })
	s.mu.Lock()
	for k, r := range s.byName {
		if r == rec && k != name {
			delete(s.byName, k)
		}
	}
	for k, r := range s.byID {
		if r == rec && k != id {
			delete(s.byID, k)
		}
	}
	s.byName[name] = rec
	if id != "" {
		s.byID[id] = rec
	}
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a session and persists the registry, purging EVERY id-index
// entry that points at the record — its live session_id and any retired chain
// id still indexed — so a deleted session can't be resolved by a stale id.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	if rec := s.byName[name]; rec != nil {
		for k, r := range s.byID {
			if r == rec {
				delete(s.byID, k)
			}
		}
	}
	delete(s.byName, name)
	s.mu.Unlock()
	return s.flush()
}

// Rename changes a session's name (its lookup key), keeping the same durable
// session_id. Errors if old is unknown or the new name is already taken.
func (s *Store) Rename(old, newName string) error {
	s.mu.Lock()
	rec, ok := s.byName[old]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("no session named %q", old)
	}
	if _, taken := s.byName[newName]; taken && newName != old {
		s.mu.Unlock()
		return fmt.Errorf("name %q is already taken", newName)
	}
	delete(s.byName, old)
	rec.Mutate(func(r *Session) { r.Name = newName })
	s.byName[newName] = rec
	s.mu.Unlock()
	return s.flush()
}

// ForgetID drops a stale session_id from the id index — used after a compact/
// clear rotates a record onto a new session_id (its old id becomes a prior id and
// must no longer resolve to the live record). The record itself stays, indexed by
// its new id via Put. No-op if the id isn't a current index entry.
func (s *Store) ForgetID(oldID string) error {
	s.mu.Lock()
	delete(s.byID, oldID)
	s.mu.Unlock()
	return nil
}

// flush writes the registry atomically (temp file + rename).
func (s *Store) flush() error {
	s.mu.RLock()
	recs := make([]*Session, 0, len(s.byName))
	for _, rec := range s.byName {
		recs = append(recs, rec)
	}
	s.mu.RUnlock()
	// Marshal COPIES, not the live records: the store's map lock says nothing about
	// a record's fields, so encoding a shared pointer races any concurrent field
	// write (an in-place PriorIDs/Jobs append can corrupt the encoder mid-walk).
	// Each snapshot is taken under that record's own lock and released before we
	// encode, so flush never holds a record lock while doing I/O.
	list := make([]*Session, 0, len(recs))
	for _, rec := range recs {
		list = append(list, rec.Snapshot())
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	// A UNIQUE temp file per flush, not a fixed "<path>.tmp": concurrent flushes
	// (two Puts/Inserts racing) would share the fixed name and one's rename would
	// yank the file out from under the other's write. Each flush snapshots the
	// whole registry, so whichever rename lands last leaves a complete, current
	// file either way.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), filepath.Base(s.path)+".tmp*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

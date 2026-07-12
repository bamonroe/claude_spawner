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
	// Self-heal phantom duplicates: a folder may hold at most one local
	// (non-sandbox) session, but older binaries (and unguarded adopt paths) could
	// register a second record for a dir that already had one — e.g. adopting a
	// stale on-disk session_id. Collapse any such duplicates on load, keeping the
	// primary and dropping the rest, so the list self-cleans on the next restart.
	if n := s.dedupeLocal(); n > 0 {
		if err := s.flush(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// dedupeLocal collapses duplicate local (non-sandbox) records that share a
// directory down to a single primary, dropping the others from both indices.
// Returns how many records it removed. The primary is the most "real" record for
// the folder: a Started session beats a not-started one, an explicit host-target
// beats an empty target (registerDiscovered leaves Target empty; a spawned/typed
// session sets it), and ties break on the lexicographically-first name (the base
// "<dir>", not the deduped "<dir>-2"). Caller holds no lock (invoked from
// OpenStore before the store is shared) and is responsible for flushing.
func (s *Store) dedupeLocal() (removed int) {
	byDir := map[string][]*Session{}
	for _, rec := range s.byName {
		if rec.Target == TargetSandbox {
			continue // sandbox sessions are keyed separately; not a dir duplicate
		}
		byDir[rec.Dir] = append(byDir[rec.Dir], rec)
	}
	for _, recs := range byDir {
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
			if rec.SessionID != "" && s.byID[rec.SessionID] == rec {
				delete(s.byID, rec.SessionID)
			}
			removed++
		}
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
	var best *Session
	for _, rec := range s.byName {
		if rec.Dir == dir && (best == nil || rec.Name < best.Name) {
			best = rec
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
	var best *Session
	for _, rec := range s.byName {
		if rec.Dir != dir {
			continue
		}
		match := rec.Host == host && rec.Target != TargetSandbox
		if host == "" {
			match = rec.Target == TargetSandbox
		}
		if match && (best == nil || rec.Name < best.Name) {
			best = rec
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

// List returns all sessions sorted by name.
func (s *Store) List() []*Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Session, 0, len(s.byName))
	for _, rec := range s.byName {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Put inserts or updates a session and persists the registry.
func (s *Store) Put(rec *Session) error {
	s.mu.Lock()
	s.byName[rec.Name] = rec
	if rec.SessionID != "" {
		s.byID[rec.SessionID] = rec
	}
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a session and persists the registry.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	if rec := s.byName[name]; rec != nil {
		delete(s.byID, rec.SessionID)
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
	rec.Name = newName
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
	list := make([]*Session, 0, len(s.byName))
	for _, rec := range s.byName {
		list = append(list, rec)
	}
	s.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

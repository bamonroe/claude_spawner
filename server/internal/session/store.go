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
	path   string
	mu     sync.RWMutex
	byName map[string]*Session
}

// OpenStore loads (or initializes) the registry at path.
func OpenStore(path string) (*Store, error) {
	s := &Store{path: path, byName: map[string]*Session{}}
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
		s.byName[rec.Name] = rec
	}
	return s, nil
}

// Get returns the session by name, or nil.
func (s *Store) Get(name string) *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[name]
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
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a session and persists the registry.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
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

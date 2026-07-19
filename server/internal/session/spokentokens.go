package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/bam/claude_spawner/server/internal/spoken"
)

// SpokenTokenStore is a concurrency-safe, file-backed registry of the app-managed
// spoken tokens (wake/end/speak phrases + optional detector models). Like the host
// and profile catalogues the app is the source of truth: it is mutated only via the
// settings wire messages (put/delete), persisted atomically so it survives
// restarts, and re-broadcast to every client on change. Records are keyed by Name
// with last-writer-wins arbitration over UpdatedAt (see hosts.go for the shape).
type SpokenTokenStore struct {
	path   string
	mu     sync.RWMutex
	byName map[string]*spoken.Token
	tombs  tombstones
}

// OpenSpokenTokenStore loads (or initializes) the spoken-token registry at path.
// On a fresh install (no file yet) it writes the seed — the built-in "hey buddy"
// wake family + the "beep" end token — so a new deployment behaves like the old
// hardcoded wake handling; after that the app owns the list (any put/delete
// flushes the file, so this never re-seeds and a delete sticks).
func OpenSpokenTokenStore(path string, seed []*spoken.Token) (*SpokenTokenStore, error) {
	s := &SpokenTokenStore{path: path, byName: map[string]*spoken.Token{}}
	tombs, err := loadTombstones(path)
	if err != nil {
		return nil, err
	}
	s.tombs = tombs
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			for _, t := range seed {
				if t != nil && t.Name != "" {
					s.byName[t.Name] = t
				}
			}
			if len(s.byName) > 0 {
				if err := s.flush(); err != nil {
					return nil, err
				}
			}
			return s, nil
		}
		return nil, err
	}
	var list []*spoken.Token
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse spoken tokens %s: %w", path, err)
	}
	for _, t := range list {
		if t.Name != "" {
			s.byName[t.Name] = t
		}
	}
	return s, nil
}

// List returns all configured tokens, sorted by name.
func (s *SpokenTokenStore) List() []*spoken.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*spoken.Token, 0, len(s.byName))
	for _, t := range s.byName {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the configured token by name, or nil.
func (s *SpokenTokenStore) Get(name string) *spoken.Token {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[name]
}

// Put upserts a token by name and persists, arbitrating last-writer-wins: an
// incoming token whose UpdatedAt is older than the stored record's — or not newer
// than a tombstone from a more-recent delete — is rejected with ErrStale (the
// gateway then re-broadcasts its newer record). A strictly-newer add resurrects a
// tombstoned key.
func (s *SpokenTokenStore) Put(t *spoken.Token) error {
	if t == nil || t.Name == "" {
		return fmt.Errorf("spoken token needs a name")
	}
	s.mu.Lock()
	if s.tombs == nil {
		s.tombs = tombstones{}
	}
	if cur := s.byName[t.Name]; cur != nil {
		if t.UpdatedAt < cur.UpdatedAt {
			s.mu.Unlock()
			return ErrStale
		}
	} else if s.tombs.blocksAdd(t.Name, t.UpdatedAt) {
		s.mu.Unlock()
		return ErrStale
	}
	s.tombs.clearIfOlder(t.Name, t.UpdatedAt)
	s.byName[t.Name] = t
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a token by name (tombstoning it at updatedAt) and persists. A
// delete older than the stored record is rejected with ErrStale.
func (s *SpokenTokenStore) Delete(name string, updatedAt int64) error {
	s.mu.Lock()
	if s.tombs == nil {
		s.tombs = tombstones{}
	}
	if cur := s.byName[name]; cur != nil && updatedAt < cur.UpdatedAt {
		s.mu.Unlock()
		return ErrStale
	}
	delete(s.byName, name)
	s.tombs.tombstone(name, updatedAt)
	s.mu.Unlock()
	return s.flush()
}

// flush writes the registry atomically (temp file + rename), plus its tombstones.
func (s *SpokenTokenStore) flush() error {
	s.mu.RLock()
	list := make([]*spoken.Token, 0, len(s.byName))
	for _, t := range s.byName {
		list = append(list, t)
	}
	tombs := make(tombstones, len(s.tombs))
	for k, v := range s.tombs {
		tombs[k] = v
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
	if err := os.Rename(tmp, s.path); err != nil {
		return err
	}
	return flushTombstones(s.path, tombs)
}

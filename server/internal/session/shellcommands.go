package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ShellCommand is one entry in the pre-configured shell-command catalogue: a
// spoken alias bound to a fixed command line on a target host. It is the safety
// boundary for the shell-token feature — while detached, a shell token opens a
// parsing surface that can only resolve to one of these records, so arbitrary
// spoken shell is never runnable. The app is the source of truth (all editing
// happens in-app); the server persists the catalogue so it survives restarts and
// is shared across clients.
//
// # Argument template syntax
//
// Command is a template, not a shell string to be interpolated blindly. The
// spoken remainder of the utterance is split into whitespace-separated arguments
// and substituted into these placeholders:
//
//	$1 … $9   the Nth spoken argument; an absent argument substitutes to empty
//	$*        every remaining argument not consumed by a $1..$9, space-joined
//	$$        a literal dollar sign
//
// A "$" followed by anything else is left as-is. Substituted values are
// shell-quoted by the executor, so an argument can never inject an operator,
// a second command, or a redirect — the shape of the command line is fixed by
// whoever configured the catalogue entry. A template with no placeholders simply
// ignores any spoken arguments.
type ShellCommand struct {
	// Name is the spoken alias (what the user says after the shell token), e.g.
	// "disk space" or "restart caddy". It is the record key.
	Name string `json:"name"`
	// Command is the command-line template run on the target host, using the
	// argument syntax documented above.
	Command string `json:"command"`
	// Dir is the working directory to run in; empty means the login default.
	Dir string `json:"dir,omitempty"`
	// Host optionally overrides the current shell target host for this one command
	// (a HostStore name). Empty means "run on whatever the target is set to".
	Host string `json:"host,omitempty"`
	// UpdatedAt is the client-stamped last-edit time in unix MILLISECONDS, driving
	// last-writer-wins arbitration exactly as on Host (see hosts.go).
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// ShellCommandStore is a concurrency-safe, file-backed registry of the
// app-managed ShellCommand catalogue, mutated only via the settings wire
// messages (put/delete) and persisted atomically. Records are keyed by Name with
// last-writer-wins arbitration over UpdatedAt (see hosts.go for the shape).
type ShellCommandStore struct {
	path   string
	mu     sync.RWMutex
	byName map[string]*ShellCommand
	tombs  tombstones
}

// OpenShellCommandStore loads (or initializes) the shell-command catalogue at
// path. A missing file means an empty catalogue — nothing is seeded, because any
// seed would be a runnable command the user never configured.
func OpenShellCommandStore(path string) (*ShellCommandStore, error) {
	s := &ShellCommandStore{path: path, byName: map[string]*ShellCommand{}}
	tombs, err := loadTombstones(path)
	if err != nil {
		return nil, err
	}
	s.tombs = tombs
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*ShellCommand
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse shell commands %s: %w", path, err)
	}
	for _, c := range list {
		if c.Name != "" {
			s.byName[c.Name] = c
		}
	}
	return s, nil
}

// List returns all configured shell commands, sorted by name.
func (s *ShellCommandStore) List() []*ShellCommand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*ShellCommand, 0, len(s.byName))
	for _, c := range s.byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the configured shell command by name, or nil.
func (s *ShellCommandStore) Get(name string) *ShellCommand {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[name]
}

// Put upserts a shell command by name and persists, arbitrating last-writer-wins:
// an incoming record whose UpdatedAt is older than the stored one's — or not newer
// than a tombstone from a more-recent delete — is rejected with ErrStale (the
// gateway then re-broadcasts its newer record). A strictly-newer add resurrects a
// tombstoned key.
func (s *ShellCommandStore) Put(c *ShellCommand) error {
	if c == nil || c.Name == "" {
		return fmt.Errorf("shell command needs a name")
	}
	if c.Command == "" {
		return fmt.Errorf("shell command %q needs a command", c.Name)
	}
	s.mu.Lock()
	if s.tombs == nil {
		s.tombs = tombstones{}
	}
	if cur := s.byName[c.Name]; cur != nil {
		if c.UpdatedAt < cur.UpdatedAt {
			s.mu.Unlock()
			return ErrStale
		}
	} else if s.tombs.blocksAdd(c.Name, c.UpdatedAt) {
		s.mu.Unlock()
		return ErrStale
	}
	s.tombs.clearIfOlder(c.Name, c.UpdatedAt)
	s.byName[c.Name] = c
	s.mu.Unlock()
	return s.flush()
}

// Delete removes a shell command by name (tombstoning it at updatedAt) and
// persists. A delete older than the stored record is rejected with ErrStale.
func (s *ShellCommandStore) Delete(name string, updatedAt int64) error {
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

// flush writes the catalogue atomically (temp file + rename), plus its tombstones.
func (s *ShellCommandStore) flush() error {
	s.mu.RLock()
	list := make([]*ShellCommand, 0, len(s.byName))
	for _, c := range s.byName {
		list = append(list, c)
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

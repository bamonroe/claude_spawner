package gateway

import (
	"encoding/json"
	"os"
	"sort"
	"sync"
)

// inflightTracker persists the set of sessions (by session_id) that have a turn
// running right now. Turns don't survive a server restart (the claude child is in
// this process's cgroup and the jobs map is in memory), so if the server dies
// mid-turn the app would otherwise wait forever on a reply. On the next start we
// load the leftover set and, when the app re-attaches to one of those sessions,
// tell it the turn was interrupted (its result, if any, is already in the
// on-disk transcript the app reloads on attach). Keyed by session_id, so a rename
// across the restart can't lose the flag.
type inflightTracker struct {
	mu   sync.Mutex
	path string
	set  map[string]bool
}

// newInflightTracker loads any sessions left in-flight by a previous run
// (returned as `prior` — these were interrupted by the restart), then resets the
// on-disk set to empty for this fresh process.
func newInflightTracker(path string) (t *inflightTracker, prior map[string]bool) {
	prior = map[string]bool{}
	if data, err := os.ReadFile(path); err == nil {
		var names []string
		if json.Unmarshal(data, &names) == nil {
			for _, n := range names {
				prior[n] = true
			}
		}
	}
	t = &inflightTracker{path: path, set: map[string]bool{}}
	t.flush() // nothing is running yet in this process
	return t, prior
}

func (t *inflightTracker) add(sessID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.set[sessID] {
		t.set[sessID] = true
		t.flush()
	}
}

func (t *inflightTracker) remove(sessID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.set[sessID] {
		delete(t.set, sessID)
		t.flush()
	}
}

// flush writes the set atomically. Called with t.mu held (or pre-share).
func (t *inflightTracker) flush() {
	if t.path == "" {
		return
	}
	names := make([]string, 0, len(t.set))
	for n := range t.set {
		names = append(names, n)
	}
	sort.Strings(names)
	data, err := json.Marshal(names)
	if err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, t.path)
	}
}

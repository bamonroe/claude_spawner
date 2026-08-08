package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// jsonStore is the shared machinery behind the on-disk caches (DigestCache,
// UsageCache): a string-keyed map held in memory, persisted to one JSON file.
//
// The load-bearing property is that MUTATING IT NEVER TOUCHES THE DISK. Every
// Put used to marshal the entire map and do a CreateTemp+write+rename while
// holding the mutex, so the 8-way digest sweep serialized behind N whole-file
// rewrites and every turn end paid one on a latency-sensitive path. Here a
// mutation only edits the map and bumps a version; a single background writer
// coalesces versions and writes at most one file per debounce window, marshalling
// a snapshot taken outside the lock.
//
// Because exactly one goroutine ever writes the file, the lost-update bug that
// forced the old marshal-and-write-under-the-lock design cannot recur: snapshots
// are taken in version order and land in that order. Losing the last few
// milliseconds of mutations to a crash is fine — both caches are pure
// accelerators whose entries recompute.
//
// A store with an empty path is memory-only (tests): no writer goroutine, no file.
type jsonStore[T any] struct {
	path     string
	prefix   string // temp-file prefix for the atomic replace
	debounce time.Duration

	mu      sync.Mutex
	cond    *sync.Cond
	entries map[string]T
	version uint64 // bumped by every mutation
	written uint64 // highest version durably on disk

	wake chan struct{} // buffered 1: "there is work"
	now  chan struct{} // buffered 1: "skip the debounce"
}

// flushDebounce is how long a mutation waits for company before the writer
// persists. Long enough that a sweep's worth of Puts collapses into one file
// write, short enough that a restart moments later keeps almost everything.
const flushDebounce = 250 * time.Millisecond

// openJSONStore loads path into a new store, or returns an empty one if the file
// is missing or unreadable — losing the cache costs speed, never correctness.
func openJSONStore[T any](path, prefix string) *jsonStore[T] {
	s := &jsonStore[T]{
		path:     path,
		prefix:   prefix,
		debounce: flushDebounce,
		entries:  map[string]T{},
		wake:     make(chan struct{}, 1),
		now:      make(chan struct{}, 1),
	}
	s.cond = sync.NewCond(&s.mu)
	if path == "" {
		return s
	}
	if data, err := os.ReadFile(path); err == nil {
		var loaded map[string]T
		if json.Unmarshal(data, &loaded) == nil && loaded != nil {
			s.entries = loaded
		}
	}
	go s.writeLoop()
	return s
}

// get returns the entry for key, if present.
func (s *jsonStore[T]) get(key string) (T, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.entries[key]
	return v, ok
}

// update runs fn against the entry for key under the lock. fn returns the new
// value and whether anything changed; only a real change schedules a flush, so a
// re-Put of an identical entry stays free.
func (s *jsonStore[T]) update(key string, fn func(cur T, found bool) (T, bool)) {
	s.mu.Lock()
	cur, found := s.entries[key]
	next, changed := fn(cur, found)
	if !changed {
		s.mu.Unlock()
		return
	}
	s.entries[key] = next
	s.version++
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default: // already pending; the writer will pick up the newest version
	}
}

// sync blocks until everything mutated so far is on disk. Tests use it; nothing
// on a request path should.
func (s *jsonStore[T]) sync() {
	if s == nil || s.path == "" {
		return
	}
	s.mu.Lock()
	want := s.version
	s.mu.Unlock()
	select {
	case s.now <- struct{}{}:
	default:
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for s.written < want {
		s.cond.Wait()
	}
}

// writeLoop is the single writer: wake, wait out the debounce window so a burst
// of mutations collapses into one file, then persist whatever the map holds now.
func (s *jsonStore[T]) writeLoop() {
	for range s.wake {
		timer := time.NewTimer(s.debounce)
		select {
		case <-timer.C:
		case <-s.now:
			timer.Stop()
		}
		s.flush()
	}
}

func (s *jsonStore[T]) flush() {
	s.mu.Lock()
	if s.written >= s.version {
		s.mu.Unlock()
		return
	}
	version := s.version
	snapshot := make(map[string]T, len(s.entries))
	for k, v := range s.entries {
		snapshot[k] = v
	}
	s.mu.Unlock()

	// Marshal and write off the lock: readers and mutators only ever contend on
	// the in-memory map.
	if data, err := json.Marshal(snapshot); err == nil {
		s.writeFile(data)
	}

	s.mu.Lock()
	if version > s.written {
		s.written = version
	}
	s.cond.Broadcast()
	s.mu.Unlock()
}

// writeFile replaces the cache file atomically, so a crash mid-write can't leave
// a truncated file that reads back as an empty cache.
func (s *jsonStore[T]) writeFile(data []byte) {
	tmp, err := os.CreateTemp(filepath.Dir(s.path), s.prefix)
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
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
	}
}

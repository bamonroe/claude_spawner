package session

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// TestSnapshotClonesEverySliceField is the invariant behind Store.flush: a
// snapshot must share NO backing array with the live record, or an in-place
// append on the record writes through into the copy the encoder is walking. It
// is reflection-driven ON PURPOSE — a new slice field added to Session that
// Snapshot forgets to clone fails this test instead of becoming a silent race.
func TestSnapshotClonesEverySliceField(t *testing.T) {
	live := &Session{
		Name: "s", SessionID: "id",
		PriorIDs:     []string{"a"},
		PendingNotes: []string{"note"},
		AgyBrainIDs:  []string{"brain"},
		Jobs:         []BackgroundJob{{ID: "j"}},
		History:      []HistorySegment{{Agent: "codex", IDs: []string{"h"}}},
	}
	cp := live.Snapshot()

	lv, cv := reflect.ValueOf(*live), reflect.ValueOf(*cp)
	typ := lv.Type()
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.Slice || !f.IsExported() {
			continue
		}
		a, b := lv.Field(i), cv.Field(i)
		if a.Len() == 0 {
			t.Fatalf("field %s: test fixture must populate every slice field so aliasing is observable", f.Name)
		}
		if a.Len() != b.Len() {
			t.Fatalf("field %s: snapshot len %d, want %d", f.Name, b.Len(), a.Len())
		}
		if a.Pointer() == b.Pointer() {
			t.Errorf("field %s: snapshot shares the live record's backing array", f.Name)
		}
	}
	// Nested slice inside History must be cloned too, not just the outer slice.
	if &live.History[0].IDs[0] == &cp.History[0].IDs[0] {
		t.Error("History[].IDs shares the live record's backing array")
	}
}

// TestConcurrentMutateAndFlush is the race the record lock exists to close: a
// turn goroutine appending to a record while the store marshals it. Run under
// -race it fails loudly without the lock (and without flush snapshotting).
func TestConcurrentMutateAndFlush(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &Session{Name: "s", SessionID: "id"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // a turn goroutine growing the rotation chain
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rec.Mutate(func(r *Session) {
				r.PriorIDs = append(r.PriorIDs, "prior")
				r.Started = true
			})
		}
	}()
	go func() { // another device changing the model / killing a job
		defer wg.Done()
		for i := 0; i < 200; i++ {
			rec.Mutate(func(r *Session) {
				r.Model = "opus"
				r.Jobs = append(r.Jobs, BackgroundJob{ID: "j"})
			})
		}
	}()
	go func() { // the store persisting all the while
		defer wg.Done()
		for i := 0; i < 200; i++ {
			if err := store.Put(rec); err != nil {
				t.Errorf("put: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	if got := len(rec.PriorIDs); got != 200 {
		t.Errorf("PriorIDs = %d, want 200 (a lost update means the lock isn't held)", got)
	}
	if got := len(rec.Jobs); got != 200 {
		t.Errorf("Jobs = %d, want 200", got)
	}
}

// TestRenameIsLockedAgainstFieldWrites covers the one store-mediated field write
// (Store.Rename mutates rec.Name) racing a record mutation from a turn.
func TestRenameIsLockedAgainstFieldWrites(t *testing.T) {
	store, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &Session{Name: "a", SessionID: "id"}
	if err := store.Put(rec); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			rec.Mutate(func(r *Session) { r.PendingSeed = "seed" })
		}
	}()
	go func() {
		defer wg.Done()
		names := []string{"a", "b"}
		for i := 0; i < 100; i++ {
			if err := store.Rename(names[i%2], names[(i+1)%2]); err != nil {
				t.Errorf("rename: %v", err)
				return
			}
		}
	}()
	wg.Wait()
}

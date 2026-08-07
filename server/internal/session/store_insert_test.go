package session

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestInsertConcurrentAdoptionsNeverDropARecord is the regression test for the
// name-allocation TOCTOU: resolving a unique name and Putting as two steps let
// two concurrent adoptions in one dir both compute "spawner", so the second Put
// silently overwrote the first in byName — the record vanished from List()
// (the missing-names symptom) while staying live in byID. Insert allocates and
// indexes under one lock, so every record must survive, each under a distinct
// name, and stay resolvable by its id.
func TestInsertConcurrentAdoptionsNeverDropARecord(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := &Session{Name: "spawner", Dir: "/data/claude_spawner", SessionID: fmt.Sprintf("id-%d", i)}
			got, err := s.Insert(rec)
			if err != nil {
				t.Errorf("insert %d: %v", i, err)
			} else if got != rec {
				t.Errorf("insert %d: distinct ids must each get their own record", i)
			}
		}(i)
	}
	wg.Wait()
	if got := s.List(); len(got) != n {
		names := make([]string, 0, len(got))
		for _, r := range got {
			names = append(names, r.Snapshot().Name)
		}
		t.Fatalf("List() = %d records, want %d: %v", len(got), n, names)
	}
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("id-%d", i)
		rec := s.GetBySessionID(id)
		if rec == nil {
			t.Fatalf("record %s unresolvable by id", id)
		}
		name := rec.Snapshot().Name
		if seen[name] {
			t.Fatalf("name %q allocated twice", name)
		}
		seen[name] = true
		if name != "spawner" && !strings.HasPrefix(name, "spawner-") {
			t.Errorf("record %s got name %q, want spawner or spawner-N", id, name)
		}
	}
}

// TestInsertIsIdempotentOnOwnedID: concurrent adoptions of ONE session_id (the
// discover-sheet double-tap, or two devices racing) must converge on a single
// record — the second Insert returns the existing owner instead of minting the
// phantom duplicate. Owned covers retired ids too: an id in PriorIDs blocks a
// fresh insert the same way a live id does.
func TestInsertIsIdempotentOnOwnedID(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	recs := make([]*Session, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := s.Insert(&Session{Name: "proj", Dir: "/data/proj", SessionID: "the-id"})
			if err != nil {
				t.Errorf("insert %d: %v", i, err)
			}
			recs[i] = got
		}(i)
	}
	wg.Wait()
	if got := s.List(); len(got) != 1 {
		t.Fatalf("List() = %d records, want 1", len(got))
	}
	for i := 1; i < n; i++ {
		if recs[i] != recs[0] {
			t.Fatalf("insert %d returned a different record than insert 0", i)
		}
	}
	// A RETIRED id must refuse a fresh insert too.
	recs[0].Mutate(func(r *Session) {
		r.PriorIDs = append(r.PriorIDs, r.SessionID)
		r.SessionID = "rotated-id"
	})
	got, err := s.Insert(&Session{Name: "proj", Dir: "/data/proj", SessionID: "the-id"})
	if err != nil {
		t.Fatal(err)
	}
	if got != recs[0] {
		t.Fatal("insert of a retired id minted a new record instead of returning its owner")
	}
}

// TestDeletePurgesEveryChainID: deleting a session must remove every id-index
// entry that points at it — including ids retired by clear/compress that Put
// once indexed — so a deleted session can't be resolved by any stale handle.
func TestDeletePurgesEveryChainID(t *testing.T) {
	s, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatal(err)
	}
	rec := &Session{Name: "proj", Dir: "/data/proj", SessionID: "old-id"}
	if _, err := s.Insert(rec); err != nil {
		t.Fatal(err)
	}
	// Rotate the id the way clear/compress does, re-indexing via Put.
	rec.Mutate(func(r *Session) {
		r.PriorIDs = append(r.PriorIDs, r.SessionID)
		r.SessionID = "new-id"
	})
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}
	// Put purges the rotated-away key on its own now.
	if s.GetBySessionID("old-id") != nil {
		t.Error("Put left the rotated-away id in the index")
	}
	if err := s.Delete("proj"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old-id", "new-id"} {
		if s.GetBySessionID(id) != nil {
			t.Errorf("deleted session still resolvable by id %q", id)
		}
	}
}

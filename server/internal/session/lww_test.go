package session

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestHostLWWArbitration covers the updated_at last-writer-wins rules on HostStore:
// an older upsert is rejected, a newer one wins, a delete tombstone blocks a stale
// re-add, and a re-add newer than the tombstone resurrects the record.
func TestHostLWWArbitration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	hs, err := OpenHostStore(path)
	if err != nil {
		t.Fatal(err)
	}

	// Seed a record at t=100.
	if err := hs.Put(&Host{Name: "work", Address: "10.0.0.1", UpdatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	// An OLDER upsert (t=50) is rejected as stale and does not clobber the record.
	if err := hs.Put(&Host{Name: "work", Address: "STALE", UpdatedAt: 50}); !errors.Is(err, ErrStale) {
		t.Fatalf("older upsert should be ErrStale, got %v", err)
	}
	if got := hs.Get("work"); got == nil || got.Address != "10.0.0.1" {
		t.Fatalf("stale upsert clobbered the record: %+v", got)
	}
	// A NEWER upsert (t=200) wins.
	if err := hs.Put(&Host{Name: "work", Address: "10.0.0.2", UpdatedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if got := hs.Get("work"); got == nil || got.Address != "10.0.0.2" {
		t.Fatalf("newer upsert did not win: %+v", got)
	}

	// Delete at t=300 tombstones the key.
	if err := hs.Delete("work", 300); err != nil {
		t.Fatal(err)
	}
	if hs.Get("work") != nil {
		t.Fatal("delete did not remove the record")
	}
	// A stale re-add (t=250, older than the tombstone) is ignored.
	if err := hs.Put(&Host{Name: "work", Address: "ZOMBIE", UpdatedAt: 250}); !errors.Is(err, ErrStale) {
		t.Fatalf("re-add older than tombstone should be ErrStale, got %v", err)
	}
	if hs.Get("work") != nil {
		t.Fatal("tombstone failed to block a stale re-add")
	}
	// A re-add NEWER than the tombstone (t=400) resurrects the record and clears it.
	if err := hs.Put(&Host{Name: "work", Address: "REBORN", UpdatedAt: 400}); err != nil {
		t.Fatal(err)
	}
	if got := hs.Get("work"); got == nil || got.Address != "REBORN" {
		t.Fatalf("newer re-add did not resurrect: %+v", got)
	}

	// Tombstone persistence: it survives a reopen and still blocks a stale re-add.
	if err := hs.Delete("work", 500); err != nil {
		t.Fatal(err)
	}
	hs2, err := OpenHostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := hs2.Put(&Host{Name: "work", Address: "STALE2", UpdatedAt: 450}); !errors.Is(err, ErrStale) {
		t.Fatalf("reloaded tombstone should still block stale re-add, got %v", err)
	}
	// A stale delete (older than the current record) is rejected.
	if err := hs2.Put(&Host{Name: "keep", Address: "x", UpdatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := hs2.Delete("keep", 500); !errors.Is(err, ErrStale) {
		t.Fatalf("delete older than the record should be ErrStale, got %v", err)
	}
	if hs2.Get("keep") == nil {
		t.Fatal("stale delete removed a newer record")
	}
}

// TestProfileLWWArbitration covers the same rules on ProfileRegistry.
func TestProfileLWWArbitration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	reg, err := OpenProfileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ExecProfile{Name: "p", Image: "v1", UpdatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ExecProfile{Name: "p", Image: "stale", UpdatedAt: 50}); !errors.Is(err, ErrStale) {
		t.Fatalf("older profile upsert should be ErrStale, got %v", err)
	}
	if got := reg.Get("p"); got == nil || got.Image != "v1" {
		t.Fatalf("stale upsert clobbered profile: %+v", got)
	}
	if err := reg.Put(ExecProfile{Name: "p", Image: "v2", UpdatedAt: 200}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete("p", 300); err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ExecProfile{Name: "p", Image: "zombie", UpdatedAt: 250}); !errors.Is(err, ErrStale) {
		t.Fatalf("re-add older than profile tombstone should be ErrStale, got %v", err)
	}
	if reg.Get("p") != nil {
		t.Fatal("profile tombstone failed to block stale re-add")
	}
	if err := reg.Put(ExecProfile{Name: "p", Image: "reborn", UpdatedAt: 400}); err != nil {
		t.Fatal(err)
	}
	if got := reg.Get("p"); got == nil || got.Image != "reborn" {
		t.Fatalf("newer re-add did not resurrect profile: %+v", got)
	}
}

// TestIdentityLWWArbitration covers Update LWW and delete-tombstone blocking a
// stale re-create.
func TestIdentityLWWArbitration(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenIdentityStore(filepath.Join(dir, "identities.json"), filepath.Join(dir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create("work", "bam", "pw", false, 100); err != nil {
		t.Fatal(err)
	}
	// An older Update is rejected.
	if _, err := s.Update("work", "root", false, "", 50); !errors.Is(err, ErrStale) {
		t.Fatalf("older identity update should be ErrStale, got %v", err)
	}
	if got := s.Get("work"); got.User != "bam" {
		t.Fatalf("stale update changed the identity: %+v", got)
	}
	// Delete at t=300 tombstones the name.
	if err := s.Delete("work", 300); err != nil {
		t.Fatal(err)
	}
	// A stale re-create (older than the tombstone) is rejected.
	if _, err := s.Create("work", "bam", "pw", false, 250); !errors.Is(err, ErrStale) {
		t.Fatalf("re-create older than tombstone should be ErrStale, got %v", err)
	}
	// A newer re-create resurrects the name.
	if _, err := s.Create("work", "bam", "pw", false, 400); err != nil {
		t.Fatalf("newer re-create should resurrect, got %v", err)
	}
	if s.Get("work") == nil {
		t.Fatal("newer re-create did not resurrect the identity")
	}
}

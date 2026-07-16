package session

import (
	"path/filepath"
	"testing"
)

func TestHostStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.json")
	hs, err := OpenHostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh registry seeds the loopback host so a new deployment lists it.
	if got := hs.List(); len(got) != 1 || got[0].Name != LocalHost {
		t.Fatalf("fresh store should seed %q, got %+v", LocalHost, got)
	}
	if err := hs.Put(&Host{Name: "work", Address: "100.64.0.7", User: "bam", Port: 22, KeyFile: "/home/bam/.ssh/id"}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Put(&Host{Name: "local", Address: "localhost"}); err != nil {
		t.Fatal(err)
	}
	if err := hs.Put(nil); err == nil {
		t.Fatal("nil host should error")
	}
	if err := (&HostStore{byName: map[string]*Host{}}).Put(&Host{}); err == nil {
		t.Fatal("nameless host should error")
	}

	// Sorted by name, and an upsert replaces in place (not a duplicate).
	if err := hs.Put(&Host{Name: "work", Address: "10.0.0.9"}); err != nil {
		t.Fatal(err)
	}
	// Seeded localhost + the two added hosts, sorted by name.
	got := hs.List()
	if len(got) != 3 || got[0].Name != "local" || got[1].Name != LocalHost || got[2].Name != "work" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got[2].Address != "10.0.0.9" {
		t.Fatalf("upsert didn't replace: %+v", got[2])
	}

	// Reload from disk: persistence survives a new handle.
	hs2, err := OpenHostStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if h := hs2.Get("work"); h == nil || h.Address != "10.0.0.9" || h.User != "" {
		t.Fatalf("reloaded work host wrong: %+v", h)
	}
	if h := hs2.Get("local"); h == nil || h.Address != "localhost" {
		t.Fatalf("reloaded local host wrong: %+v", h)
	}

	// Delete persists too.
	if err := hs2.Delete("work", 1); err != nil {
		t.Fatal(err)
	}
	hs3, _ := OpenHostStore(path)
	if hs3.Get("work") != nil {
		t.Fatal("delete didn't persist")
	}
	if len(hs3.List()) != 2 { // local + seeded localhost
		t.Fatalf("expected 2 hosts after delete, got %d", len(hs3.List()))
	}

	// Deleting the seeded localhost sticks — the store isn't re-seeded on reopen
	// because the file now exists.
	if err := hs3.Delete(LocalHost, 1); err != nil {
		t.Fatal(err)
	}
	hs4, _ := OpenHostStore(path)
	if hs4.Get(LocalHost) != nil {
		t.Fatalf("deleted %q was re-seeded on reopen", LocalHost)
	}
}

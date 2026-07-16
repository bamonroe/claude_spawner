package agent

import (
	"errors"
	"path/filepath"
	"testing"
)

// TestSettingsLWWArbitration covers the updated_at last-writer-wins rule on the
// provider settings overlay: an older provider_put is rejected, a newer one wins.
// (Providers have no delete, hence no tombstones.)
func TestSettingsLWWArbitration(t *testing.T) {
	reg := Default()
	s, err := OpenSettingsStore(filepath.Join(t.TempDir(), "providers.json"), reg)
	if err != nil {
		t.Fatal(err)
	}
	ag := reg.Default()
	if ag == nil {
		t.Fatal("registry has no default backend")
	}
	first := ag.DefaultModel
	// Establish a record at t=100 with an explicit valid model override.
	var other string
	for _, m := range ag.Catalog() {
		if m.Alias != first {
			other = m.Alias
			break
		}
	}
	if other == "" {
		t.Skip("default backend has only one model; nothing to arbitrate")
	}
	if err := s.Put(ag.ID, first, nil, 100); err != nil {
		t.Fatal(err)
	}
	// An older put is rejected and does not change the stored default.
	if err := s.Put(ag.ID, other, nil, 50); !errors.Is(err, ErrStale) {
		t.Fatalf("older provider put should be ErrStale, got %v", err)
	}
	if got := s.DefaultModel(ag); got != first {
		t.Fatalf("stale put changed default model: %q", got)
	}
	// A newer put wins.
	if err := s.Put(ag.ID, other, nil, 200); err != nil {
		t.Fatal(err)
	}
	if got := s.DefaultModel(ag); got != other {
		t.Fatalf("newer put did not win: %q", got)
	}
	if s.UpdatedAt(ag) != 200 {
		t.Fatalf("UpdatedAt not tracked, got %d", s.UpdatedAt(ag))
	}
}

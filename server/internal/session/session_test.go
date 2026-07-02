package session

import (
	"path/filepath"
	"regexp"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestNewSessionID(t *testing.T) {
	a, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	if !uuidRe.MatchString(a) {
		t.Errorf("not a v4 uuid: %q", a)
	}
	b, _ := NewSessionID()
	if a == b {
		t.Errorf("expected distinct ids, got %q twice", a)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %d", len(got))
	}

	rec := &Session{Name: "claude-claude", Dir: "/data/claude_claude", SessionID: "id-1"}
	if err := s.Put(rec); err != nil {
		t.Fatal(err)
	}

	// Reopen and confirm persistence.
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := s2.Get("claude-claude")
	if got == nil || got.Dir != "/data/claude_claude" || got.SessionID != "id-1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	if err := s2.Delete("claude-claude"); err != nil {
		t.Fatal(err)
	}
	if s2.Get("claude-claude") != nil {
		t.Error("expected record gone after delete")
	}
}

package session

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDeleteByIDsPurgesEverything confirms a delete wipes every on-disk trace of
// a session — its transcript, the projects/<dir>/<id>/ sidecar, and the
// per-session state dirs (tasks/file-history/session-env) — while leaving a
// dir-mate's transcript untouched.
func TestDeleteByIDsPurgesEverything(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	id := "11111111-1111-4111-8111-111111111111"
	proj := filepath.Join(home, ".claude", "projects", "-data")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	transcript := filepath.Join(proj, id+".jsonl")
	writeFile(t, transcript, `{"cwd":"/data"}`+"\n")

	// projects/<dir>/<id>/ sidecar (subagents + tool results).
	sidecar := filepath.Join(proj, id)
	writeFile(t, filepath.Join(sidecar, "tool-results", "x.json"), "{}")

	// Per-session state dirs, keyed by the bare id.
	stateDirs := make([]string, 0, len(perSessionStateDirs))
	for _, sub := range perSessionStateDirs {
		d := filepath.Join(home, ".claude", sub, id)
		writeFile(t, filepath.Join(d, "f"), "x")
		stateDirs = append(stateDirs, d)
	}

	// A dir-mate that must survive the delete.
	mate := "22222222-2222-4222-8222-222222222222"
	mateFile := filepath.Join(proj, mate+".jsonl")
	writeFile(t, mateFile, `{"cwd":"/data"}`+"\n")

	n, err := (claudeFS{}).deleteByIDs([]string{id})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("deleted count = %d, want 1", n)
	}

	gone := append([]string{transcript, sidecar}, stateDirs...)
	for _, p := range gone {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("still on disk after delete: %s (err=%v)", p, err)
		}
	}
	if _, err := os.Stat(mateFile); err != nil {
		t.Errorf("dir-mate transcript was removed: %v", err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

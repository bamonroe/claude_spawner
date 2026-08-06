package session

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestShellCommandStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shell_commands.json")
	cs, err := OpenShellCommandStore(path)
	if err != nil {
		t.Fatal(err)
	}
	// Nothing is seeded — a seed would be a runnable command nobody configured.
	if got := cs.List(); len(got) != 0 {
		t.Fatalf("fresh store should be empty, got %+v", got)
	}
	if err := cs.Put(&ShellCommand{Name: "disk space", Command: "df -h $1", Dir: "/tmp", Host: "work", UpdatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Put(&ShellCommand{Name: "uptime", Command: "uptime", UpdatedAt: 10}); err != nil {
		t.Fatal(err)
	}
	if err := cs.Put(nil); err == nil {
		t.Fatal("nil command should error")
	}
	if err := cs.Put(&ShellCommand{Command: "ls"}); err == nil {
		t.Fatal("nameless command should error")
	}
	if err := cs.Put(&ShellCommand{Name: "empty"}); err == nil {
		t.Fatal("command with no command line should error")
	}

	// Upsert replaces in place, and the list is sorted by name.
	if err := cs.Put(&ShellCommand{Name: "disk space", Command: "df -h", UpdatedAt: 20}); err != nil {
		t.Fatal(err)
	}
	got := cs.List()
	if len(got) != 2 || got[0].Name != "disk space" || got[1].Name != "uptime" {
		t.Fatalf("unexpected list: %+v", got)
	}
	if got[0].Command != "df -h" || got[0].Dir != "" || got[0].Host != "" {
		t.Fatalf("upsert didn't replace: %+v", got[0])
	}

	// Reload from disk: persistence survives a new handle.
	cs2, err := OpenShellCommandStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if c := cs2.Get("uptime"); c == nil || c.Command != "uptime" {
		t.Fatalf("reloaded uptime wrong: %+v", c)
	}
	if err := cs2.Delete("uptime", 30); err != nil {
		t.Fatal(err)
	}
	cs3, _ := OpenShellCommandStore(path)
	if cs3.Get("uptime") != nil {
		t.Fatal("delete didn't persist")
	}
	if len(cs3.List()) != 1 {
		t.Fatalf("expected 1 command after delete, got %d", len(cs3.List()))
	}
}

func TestShellCommandStoreLWW(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shell_commands.json")
	cs, err := OpenShellCommandStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.Put(&ShellCommand{Name: "deploy", Command: "deploy.sh", UpdatedAt: 100}); err != nil {
		t.Fatal(err)
	}
	// An older edit loses.
	if err := cs.Put(&ShellCommand{Name: "deploy", Command: "old.sh", UpdatedAt: 50}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale put should be ErrStale, got %v", err)
	}
	if c := cs.Get("deploy"); c.Command != "deploy.sh" {
		t.Fatalf("stale put overwrote: %+v", c)
	}
	// An older delete loses.
	if err := cs.Delete("deploy", 50); !errors.Is(err, ErrStale) {
		t.Fatalf("stale delete should be ErrStale, got %v", err)
	}
	// A newer delete wins and tombstones the key against an older re-add.
	if err := cs.Delete("deploy", 200); err != nil {
		t.Fatal(err)
	}
	if err := cs.Put(&ShellCommand{Name: "deploy", Command: "zombie.sh", UpdatedAt: 150}); !errors.Is(err, ErrStale) {
		t.Fatalf("tombstoned re-add should be ErrStale, got %v", err)
	}
	// A strictly-newer add resurrects it, and that survives a reload.
	if err := cs.Put(&ShellCommand{Name: "deploy", Command: "new.sh", UpdatedAt: 300}); err != nil {
		t.Fatal(err)
	}
	cs2, _ := OpenShellCommandStore(path)
	if c := cs2.Get("deploy"); c == nil || c.Command != "new.sh" {
		t.Fatalf("resurrect didn't persist: %+v", c)
	}
	if err := cs2.Put(&ShellCommand{Name: "deploy", Command: "stale.sh", UpdatedAt: 250}); !errors.Is(err, ErrStale) {
		t.Fatalf("tombstones/records should survive reload, got %v", err)
	}
}

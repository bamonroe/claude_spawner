package command

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestRegistryExamplesParse enforces that every command in the registry actually
// works: its canonical Example utterance must Parse to its declared Kind. If you
// add a registry entry whose phrasing Parse doesn't recognize (or you break a
// parse rule), this fails — keeping the documented command set and the parser in
// lockstep.
func TestRegistryExamplesParse(t *testing.T) {
	for _, c := range Registry {
		if got := Parse(c.Example); got.Kind != c.Kind {
			t.Errorf("Registry %q: Parse(%q).Kind = %s, want %s", c.Title, c.Example, got.Kind, c.Kind)
		}
	}
}

// TestRegistryUnique guards against duplicate entries (a copy-paste that would
// double a command in the app's list).
func TestRegistryUnique(t *testing.T) {
	seenKind := map[Kind]bool{}
	seenTitle := map[string]bool{}
	for _, c := range Registry {
		if seenKind[c.Kind] {
			t.Errorf("duplicate Kind in Registry: %s", c.Kind)
		}
		if seenTitle[c.Title] {
			t.Errorf("duplicate Title in Registry: %q", c.Title)
		}
		seenKind[c.Kind], seenTitle[c.Title] = true, true
	}
}

// TestEveryUserKindRegistered makes the registry mandatory: every user-facing
// intent Kind must have a registry entry. Add a new command Kind and forget to
// register it (so it can't be documented or shown in the app) and this fails.
// When adding a Kind, add it here and to Registry.
func TestEveryUserKindRegistered(t *testing.T) {
	userKinds := []Kind{
		Spawn, Attach, Detach, List, Kill, Status, Cancel, Stop, AbortTurn, Help, ReadLast, Clear,
	}
	registered := map[Kind]bool{}
	for _, c := range Registry {
		registered[c.Kind] = true
	}
	for _, k := range userKinds {
		if !registered[k] {
			t.Errorf("Kind %s is user-facing but missing from Registry", k)
		}
	}
}

func TestCommandsJSONUpToDate(t *testing.T) {
	want, err := RegistryJSON()
	if err != nil {
		t.Fatal(err)
	}
	_, file, _, _ := runtime.Caller(0) // .../server/internal/command/registry_test.go
	path := filepath.Join(filepath.Dir(file), "..", "..", "..", "docs", "commands.json")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read docs/commands.json: %v (run `go run ./cmd/gencommands`)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("docs/commands.json is stale — run `go run ./cmd/gencommands` to regenerate")
	}
}

package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestProfileStoreLoadsFileAndResolvesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	data := `{
		"profiles": [
			{"name": "bare-metal", "target": "host"},
			{
				"name": "ollama", "target": "sandbox", "default": true,
				"mounts": ["/host/auth.json:/root/auth.json:ro"],
				"env": {"OLLAMA_BASE_URL": "http://10.0.0.8:11434"}
			}
		]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := OpenProfileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.DefaultName(); got != "ollama" {
		t.Errorf("DefaultName = %q, want ollama (the marked profile)", got)
	}
	if reg.Resolve("").Name != "ollama" {
		t.Errorf("empty name should resolve to the marked default")
	}
	if reg.Resolve("missing") != reg.Resolve("") {
		t.Errorf("unknown profile did not fall back to default")
	}
	if got := reg.Resolve("ollama").Env["OLLAMA_BASE_URL"]; got != "http://10.0.0.8:11434" {
		t.Errorf("env = %q", got)
	}
}

// TestProfileStoreFirstRunSeeds verifies a missing file is seeded and persisted,
// with the first profile treated as default when none is explicitly marked.
func TestProfileStoreFirstRunSeeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	seed := []ExecProfile{{Name: "bare-metal", Target: TargetHost, Default: true}, {Name: "sandbox", Target: TargetSandbox}}
	reg, err := OpenProfileStore(path, seed)
	if err != nil {
		t.Fatal(err)
	}
	if reg.DefaultName() != "bare-metal" {
		t.Errorf("seeded default = %q, want bare-metal", reg.DefaultName())
	}
	// The seed must have been written, so a reopen with no seed sees the same set.
	reopened, err := OpenProfileStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(reopened.List()) != 2 || reopened.Resolve("sandbox") == nil {
		t.Errorf("seed was not persisted: %d profiles", len(reopened.List()))
	}
}

func TestProfileStorePutDeleteSetDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	reg, err := OpenProfileStore(path, []ExecProfile{{Name: "a", Default: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Put(ExecProfile{Name: "b", Target: TargetSandbox}); err != nil {
		t.Fatal(err)
	}
	if err := reg.SetDefault("b"); err != nil {
		t.Fatal(err)
	}
	if reg.DefaultName() != "b" || reg.Resolve("a").Default {
		t.Errorf("SetDefault did not move the marker exclusively to b")
	}
	if err := reg.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if reg.Get("a") != nil || len(reg.List()) != 1 {
		t.Errorf("Delete left a behind")
	}
	// Persisted across reopen.
	reopened, _ := OpenProfileStore(path, nil)
	if reopened.DefaultName() != "b" || reopened.Get("a") != nil {
		t.Errorf("mutations not persisted")
	}
}

func TestProfileStoreRejectsBadEnvKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.json")
	if err := os.WriteFile(path, []byte(`[{"name":"bad","env":{"1NOPE":"x"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenProfileStore(path, nil); err == nil {
		t.Fatal("OpenProfileStore succeeded with invalid env key")
	}
}

func TestProfileEnvListSorted(t *testing.T) {
	p := &ExecProfile{Env: map[string]string{"B": "2", "A": "1"}}
	if got, want := p.envList(), []string{"A=1", "B=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envList = %v, want %v", got, want)
	}
}

func TestProfileRenderExpandsAllFields(t *testing.T) {
	p := &ExecProfile{
		Name:      "ollama",
		Image:     "img",
		HomeMount: "{{.Home}}",
		Mounts:    []string{"{{.Home}}/src:/src:rw", "{{.Dir}}:/work:rw"},
		Creds:     []string{"{{.Home}}/.secrets/{{.Session}}.json:/creds:ro"},
		Env:       map[string]string{"OLLAMA_BASE_URL": "http://{{.Vars.OllamaHost}}:11434"},
		RunArgs:   []string{"--add-host", "ollama:{{.Vars.OllamaIP}}"},
	}
	ctx := RenderContext{
		Home:    "/home/bam",
		Session: "proj",
		Dir:     "/work/proj",
		Vars:    map[string]string{"OllamaHost": "pickle.bam.net", "OllamaIP": "10.0.0.8"},
	}
	got, err := p.render(ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	checks := map[string]string{
		"HomeMount": got.HomeMount, "mount0": got.Mounts[0], "mount1": got.Mounts[1],
		"cred0": got.Creds[0], "env": got.Env["OLLAMA_BASE_URL"], "runarg1": got.RunArgs[1],
	}
	want := map[string]string{
		"HomeMount": "/home/bam", "mount0": "/home/bam/src:/src:rw", "mount1": "/work/proj:/work:rw",
		"cred0": "/home/bam/.secrets/proj.json:/creds:ro",
		"env":   "http://pickle.bam.net:11434", "runarg1": "ollama:10.0.0.8",
	}
	for k, w := range want {
		if checks[k] != w {
			t.Errorf("%s = %q, want %q", k, checks[k], w)
		}
	}
	if p.HomeMount != "{{.Home}}" {
		t.Errorf("render mutated the source profile: HomeMount = %q", p.HomeMount)
	}
}

func TestProfileRenderUnknownVarFailsLoud(t *testing.T) {
	p := &ExecProfile{Name: "bad", Env: map[string]string{"X": "{{.Vars.Missing}}"}}
	if _, err := p.render(RenderContext{Vars: map[string]string{}}); err == nil {
		t.Fatal("render succeeded with an undefined var; expected a hard error")
	}
}

func TestProfileForMergesVarsProfileWins(t *testing.T) {
	reg, err := NewProfileRegistry(
		ExecProfile{Name: "base", Default: true},
		ExecProfile{Name: "p", Env: map[string]string{"U": "{{.Vars.A}}-{{.Vars.B}}"}, Vars: map[string]string{"B": "prof"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	d := &Driver{Profiles: reg, GlobalVars: map[string]string{"A": "glob", "B": "glob"}}
	got, err := d.ProfileFor(&Session{Name: "s", Dir: "/d", Profile: "p"})
	if err != nil {
		t.Fatalf("ProfileFor: %v", err)
	}
	if got.Env["U"] != "glob-prof" {
		t.Errorf("merged env = %q, want %q (profile var should win)", got.Env["U"], "glob-prof")
	}
}

// TestShippedExampleProfilesLoad guards deploy/profiles.example.json so the
// documented preset can't silently rot into something the loader rejects.
func TestShippedExampleProfilesLoad(t *testing.T) {
	path := filepath.Join("..", "..", "..", "deploy", "profiles.example.json")
	reg, err := OpenProfileStore(path, nil)
	if err != nil {
		t.Fatalf("example profiles failed to load: %v", err)
	}
	locked := reg.Resolve("locked")
	if locked == nil || locked.Name != "locked" {
		t.Fatalf("example is missing a 'locked' profile; got %+v", locked)
	}
	if locked.HomeMount != "" {
		t.Errorf("'locked' profile must not carry a home mount, got %q", locked.HomeMount)
	}
	if open := reg.Resolve("open"); open == nil || open.HomeMount == "" {
		t.Errorf("'open' profile should carry a home mount, got %+v", open)
	}
}

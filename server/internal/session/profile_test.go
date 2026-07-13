package session

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadProfilesDefaultAndExtras(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	data := `{
		"profiles": [
			{
				"name": "ollama",
				"mounts": ["/host/auth.json:/root/auth.json:ro"],
				"env": {"OLLAMA_BASE_URL": "http://10.0.0.8:11434"},
				"run_args": ["--add-host", "pickle:100.64.0.7"]
			}
		]
	}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	def := ExecProfile{
		Name:    DefaultProfileName,
		Target:  TargetSandbox,
		Image:   "default-image",
		Mounts:  []string{"/default:/default:ro"},
		RunArgs: []string{"--userns=keep-id"},
	}
	reg, err := LoadProfiles(path, def)
	if err != nil {
		t.Fatal(err)
	}
	if got := reg.Resolve("").Image; got != "default-image" {
		t.Fatalf("default image = %q", got)
	}
	ollama := reg.Resolve("ollama")
	if ollama == nil {
		t.Fatal("missing ollama profile")
	}
	if ollama.Image != "default-image" {
		t.Errorf("extra profile should inherit image, got %q", ollama.Image)
	}
	if !reflect.DeepEqual(ollama.Mounts, []string{"/host/auth.json:/root/auth.json:ro"}) {
		t.Errorf("mounts = %v", ollama.Mounts)
	}
	if got := ollama.Env["OLLAMA_BASE_URL"]; got != "http://10.0.0.8:11434" {
		t.Errorf("env = %q", got)
	}
	if reg.Resolve("missing") != reg.Resolve("") {
		t.Errorf("unknown profile did not fall back to default")
	}
}

func TestLoadProfilesRejectsBadEnvKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, []byte(`[{"name":"bad","env":{"1NOPE":"x"}}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProfiles(path, ExecProfile{Name: DefaultProfileName}); err == nil {
		t.Fatal("LoadProfiles succeeded with invalid env key")
	}
}

func TestProfileEnvListSorted(t *testing.T) {
	p := &ExecProfile{Env: map[string]string{"B": "2", "A": "1"}}
	if got, want := p.envList(), []string{"A=1", "B=2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("envList = %v, want %v", got, want)
	}
}

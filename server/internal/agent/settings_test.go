package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// testReg is a two-backend registry with known model catalogues.
func testReg() *Registry {
	r := &Registry{byID: map[string]*Agent{}}
	r.register(&Agent{ID: "claude", Name: "Claude", DefaultModel: "opus", Models: []Model{
		{Alias: "opus"}, {Alias: "sonnet"}, {Alias: "fable"},
	}})
	r.register(&Agent{ID: "codex", Name: "Codex", DefaultModel: "gpt-5.5", Models: []Model{
		{Alias: "gpt-5.5"}, {Alias: "gpt-5.5-high"},
	}})
	return r
}

func TestSettingsDefaultsWhenUnset(t *testing.T) {
	reg := testReg()
	var s *SettingsStore // nil store: everything falls back to the compiled defaults
	claude, _ := reg.Get("claude")
	if got := s.DefaultModel(claude); got != "opus" {
		t.Errorf("nil store default = %q, want opus", got)
	}
	if got := len(s.VoiceModels(claude)); got != 3 {
		t.Errorf("nil store voice models = %d, want 3 (all)", got)
	}
	if !s.VoiceEnabled(claude, "sonnet") {
		t.Error("nil store should treat every model as voice-enabled")
	}
}

func TestSettingsPutOverridesDefaultAndVoice(t *testing.T) {
	reg := testReg()
	path := filepath.Join(t.TempDir(), "providers.json")
	s, err := OpenSettingsStore(path, reg)
	if err != nil {
		t.Fatal(err)
	}
	claude, _ := reg.Get("claude")
	// Default model → sonnet; voice enumerates only opus + fable (out of order in
	// the request, but stored/returned in the agent's catalogue order).
	if err := s.Put("claude", "sonnet", []string{"fable", "opus"}, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.DefaultModel(claude); got != "sonnet" {
		t.Errorf("default = %q, want sonnet", got)
	}
	vm := s.VoiceModels(claude)
	if len(vm) != 2 || vm[0].Alias != "opus" || vm[1].Alias != "fable" {
		t.Errorf("voice models = %+v, want [opus fable] in catalogue order", vm)
	}
	if s.VoiceEnabled(claude, "sonnet") {
		t.Error("sonnet should not be voice-enabled after the override")
	}

	// An empty (non-nil) voice set means none enumerated.
	if err := s.Put("claude", "", []string{}, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.DefaultModel(claude); got != "opus" {
		t.Errorf("cleared default = %q, want compiled opus", got)
	}
	if got := len(s.VoiceModels(claude)); got != 0 {
		t.Errorf("empty voice set = %d models, want 0", got)
	}

	// Overrides persist and reload.
	s2, err := OpenSettingsStore(path, reg)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(s2.VoiceModels(claude)); got != 0 {
		t.Errorf("reloaded voice set = %d, want 0", got)
	}
}

func TestSettingsPutValidates(t *testing.T) {
	reg := testReg()
	s, _ := OpenSettingsStore("", reg)
	if err := s.Put("nope", "", nil, 0); err == nil {
		t.Error("expected error for unknown backend")
	}
	if err := s.Put("claude", "gpt-5.5", nil, 0); err == nil {
		t.Error("expected error for a model of another backend")
	}
	if err := s.Put("claude", "", []string{"opus", "bogus"}, 0); err == nil {
		t.Error("expected error for a bogus voice alias")
	}
}

func TestSettingsCanonicalizesAlias(t *testing.T) {
	reg := &Registry{byID: map[string]*Agent{}}
	reg.register(&Agent{
		ID: "ollama", Aliases: []string{"opencode"}, Name: "Ollama",
		DefaultModel: "qwen2.5-coder:7b",
		Models:       []Model{{Alias: "qwen2.5-coder:7b"}, {Alias: "llama3.1:8b"}},
	})
	path := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"agent":"opencode","default_model":"llama3.1:8b","voice_models":["llama3.1:8b"],"updated_at":7}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSettingsStore(path, reg)
	if err != nil {
		t.Fatal(err)
	}
	ollama, _ := reg.Get("ollama")
	if got := s.DefaultModel(ollama); got != "llama3.1:8b" {
		t.Fatalf("legacy opencode setting default = %q, want llama3.1:8b", got)
	}
	if err := s.Put("opencode", "qwen2.5-coder:7b", nil, 8); err != nil {
		t.Fatal(err)
	}
	if got := s.DefaultModel(ollama); got != "qwen2.5-coder:7b" {
		t.Fatalf("alias put default = %q, want qwen2.5-coder:7b", got)
	}
}

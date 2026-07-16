package agent

import (
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

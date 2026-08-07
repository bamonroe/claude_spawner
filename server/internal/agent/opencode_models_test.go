package agent

import "testing"

// parseOpencodeModels turns `opencode models <provider>` stdout into a catalogue: the
// full line is the -m Flag, the alias is the provider-stripped tail, blanks and
// dups are dropped, and separators become spoken forms.
func TestParseOpencodeModels(t *testing.T) {
	out := []byte("ollama/llama3.1:8b\nollama/qwen2.5-coder:7b\n\nollama/qwen2.5-coder:14b\nollama/qwen2.5-coder:7b\n")
	got := parseOpencodeModels(out)
	if len(got) != 3 {
		t.Fatalf("want 3 models (dup dropped), got %d: %+v", len(got), got)
	}
	want := []struct{ alias, flag string }{
		{"llama3.1:8b", "ollama/llama3.1:8b"},
		{"qwen2.5-coder:7b", "ollama/qwen2.5-coder:7b"},
		{"qwen2.5-coder:14b", "ollama/qwen2.5-coder:14b"},
	}
	for i, w := range want {
		if got[i].Alias != w.alias || got[i].Flag != w.flag {
			t.Errorf("model %d: got alias=%q flag=%q, want alias=%q flag=%q", i, got[i].Alias, got[i].Flag, w.alias, w.flag)
		}
	}
	// A freshly pulled-and-configured model surfaces with a usable spoken form
	// (separators flattened to spaces) so voice-by-name has a chance.
	if len(got[2].Spoken) == 0 || got[2].Spoken[0] != "qwen2 5 coder 14b" {
		t.Errorf("spoken form for %q: got %v", got[2].Alias, got[2].Spoken)
	}
}

// A line that isn't a provider/model id is ignored (defensive against banner or
// header lines opencode might print).
func TestParseOpencodeModelsSkipsNonIDs(t *testing.T) {
	got := parseOpencodeModels([]byte("Available models:\nollama/llama3.1:8b\n  \n"))
	if len(got) != 1 || got[0].Alias != "llama3.1:8b" {
		t.Fatalf("want just the one id, got %+v", got)
	}
}

// Discovery replaces the compiled fallback everywhere the catalogue is read, and
// leaving it unset keeps the compiled list — the two mechanisms the feature promises.
func TestCatalogFallbackAndDiscovery(t *testing.T) {
	oc := ollama()
	if !oc.CanDiscover() {
		t.Fatal("Ollama should advertise discovery")
	}
	if len(oc.Catalog()) != 2 {
		t.Fatalf("pre-discovery catalog should be the compiled fallback (2), got %d", len(oc.Catalog()))
	}
	oc.SetDiscovered([]Model{{Alias: "qwen2.5-coder:14b", Flag: "ollama/qwen2.5-coder:14b"}})
	if len(oc.Catalog()) != 1 || oc.Catalog()[0].Alias != "qwen2.5-coder:14b" {
		t.Fatalf("post-discovery catalog should be the discovered list, got %+v", oc.Catalog())
	}
	if m, ok := oc.Model("qwen2.5-coder:14b"); !ok || m.Flag != "ollama/qwen2.5-coder:14b" {
		t.Fatalf("discovered model should resolve, got %+v ok=%v", m, ok)
	}
	// An empty probe result must not wipe the catalogue.
	oc.SetDiscovered(nil)
	if len(oc.Catalog()) != 1 {
		t.Fatalf("empty discovery must not clear the catalogue, got %d", len(oc.Catalog()))
	}
}

func TestZenProviderDiscovery(t *testing.T) {
	z := zen()
	if z.ID != "zen" || z.Name != "Zen" {
		t.Fatalf("zen identity = id %q name %q", z.ID, z.Name)
	}
	if len(z.DiscoverArgs) != 2 || z.DiscoverArgs[0] != "models" || z.DiscoverArgs[1] != "opencode" {
		t.Fatalf("zen discovery args = %v, want models opencode", z.DiscoverArgs)
	}
	got := parseOpencodeModels([]byte("opencode/gpt-5.5\nopencode/claude-sonnet-4-5\n"))
	if len(got) != 2 || got[0].Alias != "gpt-5.5" || got[0].Flag != "opencode/gpt-5.5" {
		t.Fatalf("zen models parsed wrong: %+v", got)
	}
}

package projects

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRank(t *testing.T) {
	root := t.TempDir()
	// Git repos (a ".git" marker makes a dir a "project").
	for _, repo := range []string{
		"claude_spawner", "caddyedit", "sfit",
		"personal/askii", "personal/color_converter", // repos inside a namespace dir
	} {
		if err := os.MkdirAll(filepath.Join(root, repo, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A plain top-level service dir (no repo) — should still be listable.
	if err := os.MkdirAll(filepath.Join(root, "jellyfin", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Noise dir with a repo inside — the noise dir must be skipped entirely.
	if err := os.MkdirAll(filepath.Join(root, "node_modules", "pkg", ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	idx := New([]string{root})

	cases := []struct {
		query    string
		wantName string // expected top result
	}{
		{"the spawner repo", "claude_spawner"},
		{"claude spawner", "claude_spawner"},
		{"caddy edit", "caddyedit"},
		{"color converter", "color_converter"},
		{"askii", "askii"},
	}
	for _, c := range cases {
		got := Rank(c.query, idx.List(1000))
		if len(got) == 0 {
			t.Errorf("Rank(%q) returned nothing", c.query)
			continue
		}
		if got[0].Name != c.wantName {
			t.Errorf("Rank(%q) top = %q, want %q (all: %v)", c.query, got[0].Name, c.wantName, names(got))
		}
	}

	// node_modules must never be surfaced.
	for _, d := range idx.List(100) {
		if d.Name == "node_modules" {
			t.Errorf("node_modules should be filtered out")
		}
	}

	// A nonsense query yields no match.
	if got := Rank("zzzznotathing", idx.List(1000)); len(got) != 0 {
		t.Errorf("expected no match, got %v", names(got))
	}
}

func names(dirs []Dir) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Name
	}
	return out
}

func TestFuzzy(t *testing.T) {
	if d := Levenshtein("get", "git"); d != 1 {
		t.Errorf("Levenshtein(get,git) = %d, want 1", d)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"get", "git", true},
		{"personel", "personal", true},
		{"git", "data", false},
		{"cat", "dog", false},
	} {
		if got := FuzzyEqual(c.a, c.b); got != c.want {
			t.Errorf("FuzzyEqual(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTokenizeCamel(t *testing.T) {
	got := tokenize("colorConverter-v2")
	want := []string{"color", "converter", "v2"}
	if len(got) != len(want) {
		t.Fatalf("tokenize = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tokenize = %v, want %v", got, want)
		}
	}
}

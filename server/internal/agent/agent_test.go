package agent

import (
	"slices"
	"testing"
)

func TestDefaultRegistryHasClaude(t *testing.T) {
	r := Default()
	c, ok := r.Get("claude")
	if !ok {
		t.Fatal("claude not registered")
	}
	if r.Default() != c {
		t.Errorf("default agent = %v, want claude", r.Default().ID)
	}
	if r.Resolve("") != c {
		t.Error("empty id should resolve to the default agent")
	}
	if r.Resolve("nope") != c {
		t.Error("unknown id should resolve to the default agent")
	}
}

func TestModelResolution(t *testing.T) {
	c, _ := Default().Get("claude")

	if m, ok := c.Model("sonnet"); !ok || m.Flag != "sonnet" {
		t.Errorf("sonnet resolved to %+v ok=%v", m, ok)
	}
	// Spoken form resolves to the canonical model.
	if m, ok := c.Model("fable five"); !ok || m.Alias != "fable" {
		t.Errorf("spoken 'fable five' resolved to %+v ok=%v", m, ok)
	}
	// Empty and unknown fall back to the default model (ok=false).
	if m, ok := c.Model(""); ok || m.Alias != c.DefaultModel {
		t.Errorf("empty resolved to %+v ok=%v, want default %q", m, ok, c.DefaultModel)
	}
	if m, ok := c.Model("gpt5"); ok || m.Alias != c.DefaultModel {
		t.Errorf("unknown resolved to %+v ok=%v, want default", m, ok)
	}
}

func TestCodexArgs(t *testing.T) {
	c, ok := Default().Get("codex")
	if !ok {
		t.Fatal("codex not registered")
	}
	if c.Bin != "codex" {
		t.Errorf("codex Bin = %q, want codex", c.Bin)
	}

	// First turn, default model: no id, model pinned, prompt after `--`.
	got := c.Args(TurnSpec{Prompt: "fix it", SessionID: "ignored", Resume: false, Model: "gpt-5.5", Bypass: true})
	want := []string{
		"exec", "--json", "--skip-git-repo-check",
		"--dangerously-bypass-approvals-and-sandbox", "-m", "gpt-5.5", "--", "fix it",
	}
	if !slices.Equal(got, want) {
		t.Errorf("codex first-turn args\n got %v\nwant %v", got, want)
	}

	// Resume turn carries the id after `resume`; reasoning preset expands to -c args.
	got = c.Args(TurnSpec{Prompt: "-rf danger", SessionID: "thread-abc", Resume: true, Model: "gpt-5.5-high"})
	want = []string{
		"exec", "resume", "thread-abc", "--json", "--skip-git-repo-check",
		"-m", "gpt-5.5", "-c", "model_reasoning_effort=high", "--", "-rf danger",
	}
	if !slices.Equal(got, want) {
		t.Errorf("codex resume args\n got %v\nwant %v", got, want)
	}
}

func TestClaudeArgsMatchLegacyPlusModel(t *testing.T) {
	c, _ := Default().Get("claude")

	// First turn, no stored model → omit --model (legacy behavior), bypass on.
	got := c.Args(TurnSpec{Prompt: "hi", SessionID: "sid", Resume: false, Bypass: true})
	want := []string{
		"-p", "hi", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--dangerously-skip-permissions",
	}
	if !slices.Equal(got, want) {
		t.Errorf("first-turn args\n got %v\nwant %v", got, want)
	}

	// Explicit fable resolves to its full model id.
	got = c.Args(TurnSpec{Prompt: "hi", SessionID: "sid", Model: "fable"})
	want = []string{
		"-p", "hi", "--output-format", "stream-json", "--verbose",
		"--session-id", "sid", "--model", "claude-fable-5",
	}
	if !slices.Equal(got, want) {
		t.Errorf("fable args\n got %v\nwant %v", got, want)
	}

	// Resume turn, explicit sonnet, no bypass.
	got = c.Args(TurnSpec{Prompt: "more", SessionID: "sid", Resume: true, Model: "sonnet"})
	want = []string{
		"-p", "more", "--output-format", "stream-json", "--verbose",
		"--resume", "sid", "--model", "sonnet",
	}
	if !slices.Equal(got, want) {
		t.Errorf("resume args\n got %v\nwant %v", got, want)
	}
}

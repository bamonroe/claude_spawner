package agent

import (
	"slices"
	"strings"
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

func TestAntigravityArgs(t *testing.T) {
	a, ok := Default().Get("antigravity")
	if !ok {
		t.Fatal("antigravity not registered")
	}
	if a.Bin != "agy" {
		t.Errorf("antigravity Bin = %q, want agy", a.Bin)
	}
	if a.SelfAssignsID {
		t.Error("antigravity should take a caller-supplied conversation id (SelfAssignsID false)")
	}

	// The caller-supplied conversation id rides every turn (create and resume look
	// identical to agy); the workspace goes via --add-dir, model pinned, prompt in
	// =form so a leading-dash dictation can't be misparsed as a flag.
	got := a.Args(TurnSpec{Prompt: "-rf be careful", SessionID: "conv-1", Dir: "/work", Model: "gemini-flash-low", Bypass: true})
	want := []string{
		"--conversation", "conv-1", "--add-dir", "/work",
		"--dangerously-skip-permissions", "--model", "Gemini 3.5 Flash (Low)",
		"--print-timeout", agyPrintTimeout, "--prompt=-rf be careful",
	}
	if !slices.Equal(got, want) {
		t.Errorf("antigravity args\n got %v\nwant %v", got, want)
	}

	// No Dir and no bypass: --add-dir and the skip-permissions flag are both omitted.
	got = a.Args(TurnSpec{Prompt: "hi", SessionID: "conv-2", Model: "gemini-pro"})
	if slices.Contains(got, "--add-dir") {
		t.Errorf("empty Dir should omit --add-dir, got %v", got)
	}
	if slices.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("no bypass should omit skip-permissions, got %v", got)
	}
}

func TestParseAgyText(t *testing.T) {
	// Clean stdout is the whole reply, trimmed; OnText fires once with it.
	var streamed string
	res, err := parseAgyText(strings.NewReader("  the answer is 42\n"), TurnCallbacks{
		OnText: func(s string) { streamed = s },
	})
	if err != nil {
		t.Fatalf("parseAgyText: %v", err)
	}
	if res.Reply != "the answer is 42" {
		t.Errorf("reply = %q, want trimmed prose", res.Reply)
	}
	if streamed != "the answer is 42" {
		t.Errorf("OnText got %q, want the reply", streamed)
	}
	if res.Usage != (Usage{}) {
		t.Errorf("agy reports no usage, got %+v", res.Usage)
	}

	// Empty stdout on a clean exit is a failed turn.
	if _, err := parseAgyText(strings.NewReader("   \n"), TurnCallbacks{}); err == nil {
		t.Error("empty agy output should error")
	}
}

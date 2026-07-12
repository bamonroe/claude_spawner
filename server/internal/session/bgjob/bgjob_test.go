package bgjob

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestScriptEmbedded(t *testing.T) {
	if !strings.Contains(Script, "spawner-job") {
		t.Fatal("embedded script missing")
	}
	// The detachment guarantees the whole design rests on: nested setsid (new
	// session/pgid) and stdin/stdout/stderr fully redirected off the turn channel.
	for _, want := range []string{"setsid nohup sh -c", "</dev/null", `>"$log" 2>&1 &`} {
		if !strings.Contains(Script, want) {
			t.Errorf("script missing detachment fragment %q", want)
		}
	}
}

func TestParseList(t *testing.T) {
	out := []byte(`[{"id":"a_1_ff","pid":42,"cmd":"sleep 1","started":100,"done":true,"exit":0},` +
		`{"id":"b_2_ee","pid":43,"cmd":"echo hi","started":101,"done":false,"exit":0}]`)
	recs, err := ParseList(out)
	if err != nil {
		t.Fatalf("ParseList: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %d", len(recs))
	}
	if recs[0].ID != "a_1_ff" || recs[0].PID != 42 || !recs[0].Done {
		t.Errorf("record 0 wrong: %+v", recs[0])
	}
	if recs[1].Done {
		t.Errorf("record 1 should be running: %+v", recs[1])
	}
}

// stagedScript writes the embedded wrapper to a temp file and returns its path.
func stagedScript(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spawner-job")
	if err := os.WriteFile(path, []byte(Script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// runHook runs the wrapper's `hook` subcommand with the given PreToolUse payload on
// stdin and returns its stdout and exit code. extraPath, if non-empty, REPLACES the
// child PATH (used to hide jq for the fallback test).
func runHook(t *testing.T, path, payload, pathEnv string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", path, "hook")
	cmd.Stdin = strings.NewReader(payload)
	if pathEnv != "" {
		cmd.Env = append(os.Environ(), "PATH="+pathEnv)
	}
	out, err := cmd.Output()
	if err == nil {
		return string(out), 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("hook run: %v", err)
	}
	return string(out), ee.ExitCode()
}

// A background bash launch must be transparently REWRITTEN (exit 0) to run detached
// through `spawner-job start`, not cancelled: the emitted PreToolUse updatedInput
// replaces the command and clears run_in_background, and the original command is
// preserved verbatim (shell-quoted) so it reaches the wrapper intact.
func TestHookRewritesBackground(t *testing.T) {
	path := stagedScript(t)
	orig := `echo "hi there"; sleep 5`
	payload := `{"tool_name":"Bash","tool_input":{"command":` + jsonStr(orig) + `,"run_in_background":true,"description":"d"}}`
	out, code := runHook(t, path, payload, "")
	if code != 0 {
		t.Fatalf("background rewrite: want exit 0, got %d (out=%s)", code, out)
	}
	var resp struct {
		HookSpecificOutput struct {
			PermissionDecision string `json:"permissionDecision"`
			UpdatedInput       struct {
				Command         string `json:"command"`
				RunInBackground bool   `json:"run_in_background"`
				Description     string `json:"description"`
			} `json:"updatedInput"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("hook output not JSON: %v\n%s", err, out)
	}
	ui := resp.HookSpecificOutput.UpdatedInput
	if ui.RunInBackground {
		t.Error("updatedInput.run_in_background should be false")
	}
	if !strings.HasPrefix(ui.Command, path+" start ") {
		t.Errorf("command not routed through spawner-job start: %q", ui.Command)
	}
	if !strings.Contains(ui.Command, orig) {
		t.Errorf("original command not preserved in %q", ui.Command)
	}
	if ui.Description != "d" {
		t.Errorf("unrelated field 'description' dropped: %q", ui.Description)
	}
	if resp.HookSpecificOutput.AdditionalContext == "" {
		t.Error("expected additionalContext explaining the detach")
	}
}

func TestHookAllowsForeground(t *testing.T) {
	path := stagedScript(t)
	fg := `{"tool_name":"Bash","tool_input":{"command":"ls","run_in_background":false}}`
	if out, code := runHook(t, path, fg, ""); code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("foreground bash: want exit 0 + no output, got code=%d out=%q", code, out)
	}
	plain := `{"tool_name":"Bash","tool_input":{"command":"ls"}}`
	if out, code := runHook(t, path, plain, ""); code != 0 || strings.TrimSpace(out) != "" {
		t.Errorf("plain bash: want exit 0 + no output, got code=%d out=%q", code, out)
	}
}

// Without jq the hook can't rebuild the tool input safely, so it must fall back to
// BLOCKING (exit 2) — enforcement never silently disappears.
func TestHookFallbackBlocksWithoutJq(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed; fallback is the only path")
	}
	// A bin dir with the utilities the no-jq path needs, but deliberately no jq.
	bin := t.TempDir()
	for _, tool := range []string{"sh", "cat", "tr", "grep", "printf"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	path := stagedScript(t)
	bg := `{"tool_name":"Bash","tool_input":{"command":"sleep 999","run_in_background":true}}`
	if _, code := runHook(t, path, bg, bin); code != 2 {
		t.Errorf("no-jq background bash: want exit 2 (blocked), got %d", code)
	}
}

// jsonStr encodes s as a JSON string literal for building test payloads.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParseListEmpty(t *testing.T) {
	recs, err := ParseList([]byte(`[]`))
	if err != nil {
		t.Fatalf("ParseList empty: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("want 0, got %d", len(recs))
	}
}

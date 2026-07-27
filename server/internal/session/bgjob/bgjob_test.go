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
func runHook(t *testing.T, path, payload, pathEnv string, hookArgs ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", append([]string{path, "hook"}, hookArgs...)...)
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

// The hook's `--owner <id>` (baked in by the server) must be threaded into the
// rewritten `start` command so the launched job is stamped with its session.
func TestHookThreadsOwner(t *testing.T) {
	path := stagedScript(t)
	payload := `{"tool_name":"Bash","tool_input":{"command":` + jsonStr("sleep 5") + `,"run_in_background":true}}`
	out, code := runHook(t, path, payload, "", "--owner", "sess-abc")
	if code != 0 {
		t.Fatalf("want exit 0, got %d (out=%s)", code, out)
	}
	var resp struct {
		HookSpecificOutput struct {
			UpdatedInput struct {
				Command string `json:"command"`
			} `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("hook output not JSON: %v\n%s", err, out)
	}
	if !strings.Contains(resp.HookSpecificOutput.UpdatedInput.Command, "start --owner ") ||
		!strings.Contains(resp.HookSpecificOutput.UpdatedInput.Command, "sess-abc") {
		t.Errorf("owner not threaded into start command: %q", resp.HookSpecificOutput.UpdatedInput.Command)
	}
}

// start --owner stamps the session into the record, and list --json reports it so
// the reconciler can attribute the job. A start without --owner leaves it empty.
func TestStartStampsOwner(t *testing.T) {
	path := stagedScript(t)
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{path}, args...)...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "SPAWNER_JOB_ROOT="+filepath.Join(dir, ".jobs"))
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("spawner-job %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run("start", "--owner", "sess-xyz", "true")
	run("start", "true") // no owner
	recs, err := ParseList([]byte(run("list", "--json")))
	if err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 jobs, got %d: %+v", len(recs), recs)
	}
	var owned, unowned int
	for _, r := range recs {
		switch r.Session {
		case "sess-xyz":
			owned++
		case "":
			unowned++
		default:
			t.Errorf("unexpected owner %q", r.Session)
		}
	}
	if owned != 1 || unowned != 1 {
		t.Errorf("want one owned + one unowned job, got owned=%d unowned=%d", owned, unowned)
	}
}

// scriptRunner returns a helper that runs the staged wrapper in a fixed working
// directory (the registry is keyed by cwd) with an isolated SPAWNER_JOB_ROOT and
// any extra env, returning trimmed stdout.
func scriptRunner(t *testing.T, extraEnv ...string) func(args ...string) string {
	t.Helper()
	path := stagedScript(t)
	dir := t.TempDir()
	env := append(os.Environ(), "SPAWNER_JOB_ROOT="+filepath.Join(dir, ".jobs"))
	env = append(env, extraEnv...)
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{path}, args...)...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("spawner-job %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
}

// wait must block on the job's process and return once it exits — the correct
// waiter primitive that replaces the leak-prone `until ! list | grep <id>` loop.
func TestWaitBlocksUntilJobExits(t *testing.T) {
	run := scriptRunner(t)
	id := run("start", "sleep 1")
	// wait returns only after the process is gone; its report names the job.
	if out := run("wait", id); !strings.Contains(out, id) {
		t.Errorf("wait output %q does not name job %s", out, id)
	}
	// After wait returns, a fresh list must see the job as done, not running.
	recs, err := ParseList([]byte(run("list", "--json")))
	if err != nil {
		t.Fatalf("parse list: %v", err)
	}
	if len(recs) != 1 || !recs[0].Done {
		t.Errorf("after wait, expected one done job, got %+v", recs)
	}
	// wait on a nonexistent id returns promptly without error.
	if out := run("wait", "no_such_id"); !strings.Contains(out, "no such job") {
		t.Errorf("wait on missing job: unexpected output %q", out)
	}
}

// A finished job must auto-expire from the registry so records can't accumulate and
// no `until ! list | grep <id>` waiter loops forever. With grace=0 a done job is
// listed once (stamping done_at) then reaped on the next list.
func TestListAutoExpiresDoneJobs(t *testing.T) {
	run := scriptRunner(t, "SPAWNER_JOB_DONE_GRACE=0")
	id := run("start", "true")
	// Give the trivial job a moment to exit so list sees it done, not running.
	run("wait", id)
	// First list observes it done and stamps done_at — still reported.
	first, err := ParseList([]byte(run("list", "--json")))
	if err != nil {
		t.Fatalf("parse first list: %v", err)
	}
	if len(first) != 1 || !first[0].Done {
		t.Fatalf("first list: want one done job, got %+v", first)
	}
	// Second list is past the (zero) grace window, so the record is reaped and gone.
	second, err := ParseList([]byte(run("list", "--json")))
	if err != nil {
		t.Fatalf("parse second list: %v", err)
	}
	if len(second) != 0 {
		t.Errorf("second list: want empty after auto-expiry, got %+v", second)
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

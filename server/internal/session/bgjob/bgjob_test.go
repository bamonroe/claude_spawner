package bgjob

import (
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

// runHook writes the embedded script to a temp file and runs its `hook`
// subcommand with the given PreToolUse payload on stdin, returning the exit code.
func runHook(t *testing.T, payload string) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spawner-job")
	if err := os.WriteFile(path, []byte(Script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	cmd := exec.Command("sh", path, "hook")
	cmd.Stdin = strings.NewReader(payload)
	err := cmd.Run()
	if err == nil {
		return 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("hook run: %v", err)
	}
	return ee.ExitCode()
}

// The hook must BLOCK (exit 2) a background bash launch — that's the enforcement
// that makes surviving-a-turn non-optional — and let a normal foreground call
// through (exit 0).
func TestHookBlocksBackground(t *testing.T) {
	bg := `{"tool_name":"Bash","tool_input":{"command":"sleep 999","run_in_background":true}}`
	if code := runHook(t, bg); code != 2 {
		t.Errorf("background bash: want exit 2 (blocked), got %d", code)
	}
	// Spacing in the payload must not let it slip past.
	bgSpaced := `{"tool_name": "Bash", "tool_input": {"command": "sleep 999", "run_in_background": true}}`
	if code := runHook(t, bgSpaced); code != 2 {
		t.Errorf("spaced background bash: want exit 2 (blocked), got %d", code)
	}
}

func TestHookAllowsForeground(t *testing.T) {
	fg := `{"tool_name":"Bash","tool_input":{"command":"ls","run_in_background":false}}`
	if code := runHook(t, fg); code != 0 {
		t.Errorf("foreground bash: want exit 0 (allowed), got %d", code)
	}
	plain := `{"tool_name":"Bash","tool_input":{"command":"ls"}}`
	if code := runHook(t, plain); code != 0 {
		t.Errorf("plain bash: want exit 0 (allowed), got %d", code)
	}
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

package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunOnTargetHost(t *testing.T) {
	dir := t.TempDir()
	d := NewDriver()
	s := &Session{Name: "t", Dir: dir, Target: TargetHost}
	out, err := d.RunOnTarget(context.Background(), s, "pwd")
	if err != nil {
		t.Fatalf("RunOnTarget: %v", err)
	}
	// The command must run in the session Dir so the dir-keyed job registry lines
	// up with Claude's own invocation.
	if strings.TrimSpace(string(out)) != dir {
		t.Errorf("pwd = %q, want %q", strings.TrimSpace(string(out)), dir)
	}
}

func TestStageJobScriptHost(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	d := NewDriver()
	s := &Session{Name: "t", Dir: dir, Target: TargetHost}
	if err := d.StageJobScript(context.Background(), s, home); err != nil {
		t.Fatalf("StageJobScript: %v", err)
	}
	path := JobScriptPath(home)
	if path != filepath.Join(home, ".spawner-jobs", "spawner-job") {
		t.Errorf("JobScriptPath = %q", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("staged script missing: %v", err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Errorf("staged script not executable: %v", fi.Mode())
	}
	// Staging is idempotent — a second call must not error.
	if err := d.StageJobScript(context.Background(), s, home); err != nil {
		t.Fatalf("re-stage: %v", err)
	}
	// The staged script actually runs (end-to-end via the host transport).
	t.Setenv("SPAWNER_JOB_ROOT", filepath.Join(home, ".spawner-jobs"))
	out, err := d.RunOnTarget(context.Background(), s, path+" list --json")
	if err != nil {
		t.Fatalf("run staged list: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "[]" {
		t.Errorf("empty list = %q, want []", strings.TrimSpace(string(out)))
	}
}

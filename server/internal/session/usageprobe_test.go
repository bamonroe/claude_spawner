package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverySkipsUsageProbeTranscripts is the structural guard behind the
// phantom-session bug: a /usage probe transcript that outlived its reap must not
// be offered as a session. The probe runs in its own directory precisely so
// discovery can recognize it, so this seeds one transcript in a probe dir and one
// in a normal dir and asserts only the real one is discovered.
func TestDiscoverySkipsUsageProbeTranscripts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	probeID := "33333333-3333-4333-8333-333333333333"
	realID := "44444444-4444-4444-8444-444444444444"
	probeDir := filepath.Join(home, usageProbeSubdir)
	projects := filepath.Join(home, ".claude", "projects")
	writeFile(t, filepath.Join(projects, "-probe", probeID+".jsonl"), `{"cwd":"`+probeDir+`"}`+"\n")
	writeFile(t, filepath.Join(projects, "-data", realID+".jsonl"), `{"cwd":"/data/real"}`+"\n")

	ds, err := DiscoverSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].SessionID != realID {
		t.Fatalf("discovered %+v, want only the real session %s", ds, realID)
	}
}

// TestIsUsageProbeDir pins the recognizer itself: it matches the probe dir on any
// host's home (it can't know a remote's $HOME), and nothing else.
func TestIsUsageProbeDir(t *testing.T) {
	yes := []string{"/home/bam/" + usageProbeSubdir, "/tmp/" + usageProbeSubdir, "/root/" + usageProbeSubdir}
	no := []string{"/home/bam", "/data/claude_spawner", "/home/bam/" + usageProbeSubdir + "/sub", usageProbeSubdir}
	for _, d := range yes {
		if !isUsageProbeDir(d) {
			t.Errorf("isUsageProbeDir(%q) = false, want true", d)
		}
	}
	for _, d := range no {
		if isUsageProbeDir(d) {
			t.Errorf("isUsageProbeDir(%q) = true, want false", d)
		}
	}
}

// TestEnsureUsageProbeDirCreatesAndFallsBack covers both halves of the dir
// resolution: it creates the probe's own subdir under the base, and a base it
// cannot create under degrades to the base itself rather than failing /usage.
func TestEnsureUsageProbeDirCreatesAndFallsBack(t *testing.T) {
	d := NewDriver() // HostExecutor, so no SSH pool: the local branch
	base := t.TempDir()
	got := d.ensureUsageProbeDir(context.Background(), base)
	want := filepath.Join(base, usageProbeSubdir)
	if got != want {
		t.Fatalf("ensureUsageProbeDir = %q, want %q", got, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Fatalf("probe dir not created: %v", err)
	}

	// A base that is a FILE can hold no subdirectory — the fallback path.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	writeFile(t, file, "x")
	if got := d.ensureUsageProbeDir(context.Background(), file); got != file {
		t.Errorf("ensureUsageProbeDir on an uncreatable base = %q, want the base %q", got, file)
	}
}

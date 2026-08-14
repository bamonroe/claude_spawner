package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPosixCommandWrapsForTheLoginShell pins the exec-layer invariant: every
// command this package hands to a remote host is parsed by sh, never by the
// account's login shell. sshd runs `<login-shell> -c <command>`, so a zsh account
// would otherwise apply NOMATCH to our globs and abort the whole command line.
// Both one-shot paths (posixCommand, used by SSHPool.Run and WriteFile) and the
// streaming path (cancelableCommand) must carry it.
func TestPosixCommandWrapsForTheLoginShell(t *testing.T) {
	got := posixCommand(`rm -rf "$HOME/x/"*/"y.jsonl"`)
	want := `sh -c 'rm -rf "$HOME/x/"*/"y.jsonl"'`
	if got != want {
		t.Errorf("posixCommand = %q, want %q", got, want)
	}
	if inner := cancelableCommand("exec claude"); !strings.Contains(inner, "sh -c ") {
		t.Errorf("cancelableCommand = %q, want an sh -c wrapper", inner)
	}
}

// TestRemoteDeleteCommandSurvivesZshNomatch is the regression test for the
// phantom-session bug: the batched remote delete globs over the opaque
// project-dir encoding, and a session with NO sidecar dir and NO per-session
// state dirs leaves most of those globs matching nothing. Under a zsh login shell
// that aborted the entire command line — the transcript survived while the caller
// was told it was purged — which is why the exec layer wraps in sh.
//
// It runs the REAL command (deleteByIDsCommand) through a real sh and a real zsh,
// against a seeded fake $HOME, and asserts the filesystem, never a count.
func TestRemoteDeleteCommandSurvivesZshNomatch(t *testing.T) {
	for _, sh := range []string{"sh", "bash", "zsh"} {
		t.Run(sh, func(t *testing.T) {
			shell, err := exec.LookPath(sh)
			if err != nil {
				t.Skipf("%s not installed", sh)
			}
			home := t.TempDir()
			// The bare case that broke: a transcript with no sidecar and no state dirs,
			// so every glob but the transcript's own matches nothing.
			bare := seedTranscript(t, home)
			// A second id with the full set of siblings, so the happy path is covered
			// in the same command — the loop must purge both.
			full := seedTranscript(t, home)
			sidecar := filepath.Join(home, ".claude", "projects", "-tmp-p", full)
			writeFile(t, filepath.Join(sidecar, "tool.json"), "{}")
			for _, sub := range perSessionStateDirs {
				mkdirAll(t, filepath.Join(home, ".claude", sub, full))
			}
			// An id that was never on disk at all: it must be silently fine, and must
			// NOT be counted as deleted.
			absent := mustSessionID(t)

			cmd := deleteByIDsCommand([]string{bare, full, absent})
			out, err := runShell(t, shell, posixCommand(cmd), home)
			if err != nil {
				t.Fatalf("%s: %v (output %q)", sh, err, out)
			}

			// Assert the filesystem — the old code reported n=1 while deleting nothing.
			for _, p := range []string{
				filepath.Join(home, ".claude", "projects", "-tmp-p", bare+".jsonl"),
				filepath.Join(home, ".claude", "projects", "-tmp-p", full+".jsonl"),
				sidecar,
				filepath.Join(home, ".claude", perSessionStateDirs[0], full),
			} {
				if _, err := os.Lstat(p); !os.IsNotExist(err) {
					t.Errorf("%s: %s still exists (stat err %v)", sh, p, err)
				}
			}
			// The count is one line per id whose transcript really existed.
			reported := strings.Fields(strings.TrimSpace(out))
			if len(reported) != 2 {
				t.Fatalf("%s: reported %v, want exactly the two seeded ids", sh, reported)
			}
			for _, id := range reported {
				if id == absent {
					t.Errorf("%s: counted %s, which was never on disk", sh, absent)
				}
			}
		})
	}
}

// TestZshNomatchWouldHaveBrokenTheDelete proves the wrapping is what saves us, not
// luck: the SAME command handed straight to zsh (as sshd would, unwrapped) deletes
// nothing. Guarded on zsh actually having NOMATCH on, so it can't fail spuriously
// on a differently-configured zsh.
func TestZshNomatchWouldHaveBrokenTheDelete(t *testing.T) {
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh not installed")
	}
	if _, err := runShell(t, shell, "echo /nonexistent-spawner-probe/*", t.TempDir()); err == nil {
		t.Skip("this zsh does not have NOMATCH set; nothing to guard")
	}
	home := t.TempDir()
	id := seedTranscript(t, home)
	path := filepath.Join(home, ".claude", "projects", "-tmp-p", id+".jsonl")
	if _, err := runShell(t, shell, deleteByIDsCommand([]string{id}), home); err == nil {
		t.Log("unwrapped zsh returned success — exactly the silent-failure shape of the bug")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("premise broken: unwrapped zsh deleted %s, so this test guards nothing", path)
	}
}

// TestLiveSSHDeleteByIDsPurgesSidecarlessTranscript is the end-to-end half: a real
// SSH round trip through Driver.DeleteSessionByIDs, deleting a transcript that has
// no sidecar and no per-session state, asserting the file is gone from disk.
// Gated on SPAWNER_SSH_LIVE=1 like the other loopback SSH tests.
func TestLiveSSHDeleteByIDsPurgesSidecarlessTranscript(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live claudeFS test")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects")
	dir, err := os.MkdirTemp(projects, "spawner-ssh-delete-*")
	if err != nil {
		t.Skipf("cannot write a test transcript under %s: %v", projects, err)
	}
	defer os.RemoveAll(dir)

	id := mustSessionID(t)
	path := filepath.Join(dir, id+".jsonl")
	writeFile(t, path, `{"type":"user","cwd":"/tmp","timestamp":"2026-08-13T00:00:00Z","message":{"content":"hi"}}`+"\n")
	// Deliberately NO sidecar dir and NO per-session state dirs — the shape that
	// left the transcript behind while reporting success.

	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	d := NewDriver()
	d.SetExec(TargetHost, SSHExecutor{Pool: pool})

	absent := mustSessionID(t)
	n, err := d.DeleteSessionByIDs(context.Background(), LocalHost, []string{id, absent})
	if err != nil {
		t.Fatalf("DeleteSessionByIDs: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("transcript %s survived the remote delete (stat err %v)", path, err)
	}
	// Only ids that truly had a transcript count — the absent one must not.
	if n != 1 {
		t.Errorf("DeleteSessionByIDs = %d, want 1 (the absent id must not be counted)", n)
	}
}

// runShell runs one command the way sshd does — `<shell> -c <command>` — with HOME
// pointed at the test's fake home, and returns its stdout.
func runShell(t *testing.T, shell, cmd, home string) (string, error) {
	t.Helper()
	c := exec.Command(shell, "-c", cmd)
	c.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
	out, err := c.Output()
	return string(out), err
}

// seedTranscript writes one transcript under home's ~/.claude/projects and returns
// its session id. Nothing else is created: no sidecar, no per-session state.
func seedTranscript(t *testing.T, home string) string {
	t.Helper()
	id := mustSessionID(t)
	dir := filepath.Join(home, ".claude", "projects", "-tmp-p")
	// writeFile (claudefs_test.go) makes the parent dirs.
	writeFile(t, filepath.Join(dir, id+".jsonl"), `{"type":"user","cwd":"/tmp/p"}`+"\n")
	return id
}

func mustSessionID(t *testing.T) string {
	t.Helper()
	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

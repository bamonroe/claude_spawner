package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These are real end-to-end checks against the host: the broker one forks the
// real `claude` binary (uses tokens/network); the sandbox ones drive a real
// container runtime (Podman by default, matching the Arch host). They're skipped
// unless SPAWNER_LIVE=1 so normal `go test` stays hermetic. Run:
//
//	SPAWNER_LIVE=1 go test ./internal/session/ -run TestLive -v
//
// Overrides: SPAWNER_LIVE_RUNTIME (default "podman"), SPAWNER_LIVE_IMAGE
// (default "spawner-sandbox:latest" — build it from ../../sandbox/Containerfile).

func liveRuntime(t *testing.T) string {
	rt := os.Getenv("SPAWNER_LIVE_RUNTIME")
	if rt == "" {
		rt = "podman"
	}
	if _, err := exec.LookPath(rt); err != nil {
		t.Skipf("%s not on PATH", rt)
	}
	return rt
}

func liveImage(t *testing.T, rt string) string {
	img := os.Getenv("SPAWNER_LIVE_IMAGE")
	if img == "" {
		img = "spawner-sandbox:latest"
	}
	if out, err := runCLI(context.Background(), rt, []string{"image", "inspect", img}); err != nil {
		t.Skipf("image %q not present (build ../../sandbox/Containerfile): %s", img, strings.TrimSpace(out))
	}
	return img
}

func TestLiveBrokerRealClaude(t *testing.T) {
	if os.Getenv("SPAWNER_LIVE") == "" {
		t.Skip("set SPAWNER_LIVE=1 to run (forks the real claude via the broker)")
	}
	dir := t.TempDir()
	// Broker with the real host claude; validator allows the temp dir.
	sock := startBroker(t, func(d string) (string, error) { return d, nil }, "claude")
	d := &Driver{Execs: map[Target]Executor{TargetHost: BrokerExecutor{Socket: sock}}, Bypass: true}

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Name: "live", Dir: dir, SessionID: id}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	reply, _, err := d.Turn(ctx, s, "Reply with exactly the token LIVEBROKEROK and nothing else.", nil, nil, nil)
	if err != nil {
		t.Fatalf("live broker turn: %v", err)
	}
	if !strings.Contains(reply, "LIVEBROKEROK") {
		t.Fatalf("reply lacked the token (broker didn't run real claude?): %q", reply)
	}
	t.Logf("broker → real claude reply: %q", reply)
}

// TestLiveSandboxContainer exercises the persistent-container lifecycle (create,
// reuse across turns, list, reconcile/remove) against a real runtime, using a
// stub claude mounted into the image so it doesn't need real credentials.
func TestLiveSandboxContainer(t *testing.T) {
	if os.Getenv("SPAWNER_LIVE") == "" {
		t.Skip("set SPAWNER_LIVE=1 to run (drives a real container runtime)")
	}
	rt := liveRuntime(t)
	img := liveImage(t, rt)
	dir := t.TempDir()
	fake := writeFakeClaude(t, "SANDBOXOK")
	se := SandboxExecutor{
		Runtime: rt,
		Image:   img,
		Bin:     "claude",
		Mounts:  []string{fake + ":/usr/local/bin/claude:ro"},
	}
	cn, err := NewContainerName()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = se.Remove(context.Background(), cn) })

	s := &Session{Name: "live", Dir: dir, SessionID: "x", Target: TargetSandbox, Container: cn}
	d := &Driver{Execs: map[Target]Executor{TargetHost: HostExecutor{Bin: "claude"}, TargetSandbox: se}, Bypass: true}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// First turn creates the container (Ensure) and execs the stub.
	reply, _, err := d.Turn(ctx, s, "hello", nil, nil, nil)
	if err != nil {
		t.Fatalf("live sandbox turn: %v", err)
	}
	if reply != "SANDBOXOK" {
		t.Fatalf("reply = %q, want SANDBOXOK", reply)
	}
	// The container persists between turns.
	if !se.running(ctx, cn) {
		t.Errorf("container %q should still be running after a turn", cn)
	}
	if _, _, err := d.Turn(ctx, s, "again", nil, nil, nil); err != nil {
		t.Fatalf("second sandbox turn (reuse): %v", err)
	}
	// It shows up in the managed list, and reconcile removes it when orphaned.
	names, err := se.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, n := range names {
		if n == cn {
			found = true
		}
	}
	if !found {
		t.Errorf("List() %v missing %q", names, cn)
	}
	removed, err := d.ReconcileContainers(ctx, map[string]bool{}) // nothing known → orphan
	if err != nil {
		t.Fatal(err)
	}
	reconciled := false
	for _, n := range removed {
		if n == cn {
			reconciled = true
		}
	}
	if !reconciled {
		t.Errorf("reconcile removed %v, expected to include %q", removed, cn)
	}
	if se.running(ctx, cn) {
		t.Errorf("container %q should be gone after reconcile", cn)
	}
	t.Logf("sandbox lifecycle ok on %s: created, reused, listed, reconciled %q", rt, cn)
}

// TestLiveSandboxRealClaude runs a REAL claude turn through the persistent
// sandbox (Podman + the Arch image), with the host claude binary and an isolated
// copy of the host credentials bind-mounted in — the full production config
// (rootless --userns=keep-id so claude runs non-root but can write the mounted
// auth). It proves a real Claude session runs end-to-end inside a sandbox.
func TestLiveSandboxRealClaude(t *testing.T) {
	if os.Getenv("SPAWNER_LIVE") == "" {
		t.Skip("set SPAWNER_LIVE=1 to run (real claude inside a real container)")
	}
	rt := liveRuntime(t)
	img := liveImage(t, rt)
	if _, err := os.Stat("/opt/claude-code/bin/claude"); err != nil {
		t.Skip("host claude bundle /opt/claude-code not present")
	}
	home := isolatedClaudeHome(t) // safe copy of the host credentials
	dir := t.TempDir()
	se := SandboxExecutor{
		Runtime: rt,
		Image:   img,
		Bin:     "claude",
		Mounts: []string{
			"/opt/claude-code:/opt/claude-code:ro",
			"/usr/bin/claude:/usr/bin/claude:ro",
			home + "/.claude:" + home + "/.claude",
			home + "/.claude.json:" + home + "/.claude.json",
		},
		// Rootless: map the host user in (claude refuses to run as root) and point
		// HOME at the mounted credentials. exec inherits both.
		RunArgs: []string{"--userns=keep-id", "-e", "HOME=" + home},
	}
	cn, err := NewContainerName()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = se.Remove(context.Background(), cn) })

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Name: "live", Dir: dir, SessionID: id, Target: TargetSandbox, Container: cn}
	d := &Driver{Execs: map[Target]Executor{TargetHost: HostExecutor{Bin: "claude"}, TargetSandbox: se}, Bypass: true}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	reply, _, err := d.Turn(ctx, s, "Reply with exactly the token SANDBOXCLAUDEOK and nothing else.", nil, nil, nil)
	if err != nil {
		t.Fatalf("real claude in sandbox: %v", err)
	}
	if !strings.Contains(reply, "SANDBOXCLAUDEOK") {
		t.Fatalf("reply lacked the token (auth/exec issue?): %q", reply)
	}
	t.Logf("sandbox → real claude reply: %q", reply)
}

// isolatedClaudeHome builds a throwaway HOME under t.TempDir() holding a copy of
// the host claude credentials, so a live test authenticates without mounting (and
// risking writes to) the developer's live ~/.claude. Skips if there's no login.
func isolatedClaudeHome(t *testing.T) string {
	t.Helper()
	real := os.Getenv("HOME")
	creds := filepath.Join(real, ".claude", ".credentials.json")
	if _, err := os.Stat(creds); err != nil {
		t.Skip("no host claude credentials (~/.claude/.credentials.json) to copy")
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	copyFile(t, creds, filepath.Join(home, ".claude", ".credentials.json"), 0o600)
	copyFile(t, filepath.Join(real, ".claude.json"), filepath.Join(home, ".claude.json"), 0o600)
	return home
}

func copyFile(t *testing.T, src, dst string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read %s: %v", src, err)
	}
	if err := os.WriteFile(dst, b, mode); err != nil {
		t.Fatalf("write %s: %v", dst, err)
	}
}

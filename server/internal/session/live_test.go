package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These are real end-to-end checks against the host: the broker one forks the
// real `claude` binary (uses tokens/network); the sandbox one drives a real
// container runtime. They're skipped unless SPAWNER_LIVE=1 so normal `go test`
// stays hermetic. Run: SPAWNER_LIVE=1 go test ./internal/session/ -run TestLive -v

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

func TestLiveSandboxContainer(t *testing.T) {
	if os.Getenv("SPAWNER_LIVE") == "" {
		t.Skip("set SPAWNER_LIVE=1 to run (drives a real container runtime)")
	}
	runtime := os.Getenv("SPAWNER_LIVE_RUNTIME")
	if runtime == "" {
		runtime = "docker"
	}
	if _, err := exec.LookPath(runtime); err != nil {
		t.Skipf("%s not on PATH", runtime)
	}
	dir := t.TempDir()
	// A stub claude inside the container (an alpine image has no real claude); the
	// point is to exercise the persistent-container lifecycle against a real
	// runtime, not claude itself.
	fake := writeFakeClaude(t, "SANDBOXOK")
	se := SandboxExecutor{
		Runtime: runtime,
		Image:   "alpine:latest",
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
	t.Logf("sandbox lifecycle ok: created, reused, listed, reconciled %q", cn)
}

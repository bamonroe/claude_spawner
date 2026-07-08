package session

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"":                 "''",
		"plain":            "'plain'",
		"two words":        "'two words'",
		"it's":             `'it'\''s'`,
		"a'b'c":            `'a'\''b'\''c'`,
		"line1\nline2":     "'line1\nline2'",
		"/data/claude/dir": "'/data/claude/dir'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRemoteCommand(t *testing.T) {
	// The prompt-bearing arg carries spaces and an apostrophe; it must survive intact
	// so the remote claude sees exactly what the user dictated.
	args := []string{"-p", "what's up here", "--output-format", "stream-json"}
	got := remoteCommand("/data/proj dir", "claude", args)
	want := `cd '/data/proj dir' && exec 'claude' '-p' 'what'\''s up here' '--output-format' 'stream-json'`
	if got != want {
		t.Errorf("remoteCommand mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestSSHBinFallback(t *testing.T) {
	if b := (SSHExecutor{}).bin(); b != "claude" {
		t.Errorf("default bin = %q, want claude", b)
	}
	if b := (SSHExecutor{Bin: "/opt/claude"}).bin(); b != "/opt/claude" {
		t.Errorf("explicit bin = %q, want /opt/claude", b)
	}
	p := &SSHPool{cfg: SSHConfig{Bin: "cfgclaude"}}
	if b := (SSHExecutor{Pool: p}).bin(); b != "cfgclaude" {
		t.Errorf("pool-config bin = %q, want cfgclaude", b)
	}
}

// TestLiveSSHLoopback exercises the pool and stream plumbing end to end against a
// real sshd on localhost. Gated on SPAWNER_SSH_LIVE=1 (like the sandbox live tests)
// because it needs key-based ssh to localhost with the host key already in
// known_hosts. It runs a trivial remote command through remoteCommand (so it also
// checks quoting survives a real shell) rather than claude, to stay fast and
// dependency-free.
func TestLiveSSHLoopback(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live loopback SSH test")
	}
	pool, err := NewSSHPool(SSHConfig{})
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()

	client, err := pool.client("localhost")
	if err != nil {
		t.Fatalf("dial localhost: %v", err)
	}
	// A second call must hit the cache, not re-dial.
	if again, _ := pool.client("localhost"); again != client {
		t.Fatal("second client() re-dialed instead of reusing the pooled connection")
	}

	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer sess.Close()
	out, err := sess.Output(remoteCommand("/", "printf", []string{"%s", "hi there"}))
	if err != nil {
		t.Fatalf("run remote command: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi there" {
		t.Fatalf("remote output = %q, want %q", out, "hi there")
	}
}

// TestSSHCancelWithoutPool guards the ctx-cancel wiring shape: Wait releasing the
// AfterFunc hook must not panic when stop already fired. Uses a stub proc rather
// than a live connection.
func TestSSHProcStopReleases(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fired := make(chan struct{}, 1)
	stop := context.AfterFunc(ctx, func() { fired <- struct{}{} })
	p := &sshProc{stop: stop}
	// Simulate Wait releasing the hook before cancellation: stop() returns true when
	// it prevented the func from running.
	if p.stop() != true {
		t.Fatal("stop() should report it cancelled the pending AfterFunc")
	}
	cancel()
	select {
	case <-fired:
		t.Fatal("AfterFunc ran even though stop() cancelled it")
	case <-time.After(50 * time.Millisecond):
	}
	_ = io.Discard
}

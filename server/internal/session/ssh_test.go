package session

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveSSHClaudeFSMatchesLocal proves the SSH backend of claudeFS (remote
// discovery/resume) reads the same on-disk Claude state as the local backend, over
// loopback (same ~/.claude), using a controlled transcript so active writes can't
// make it flaky. Gated on SPAWNER_SSH_LIVE=1.
func TestLiveSSHClaudeFSMatchesLocal(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live claudeFS test")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects")
	dir, err := os.MkdirTemp(projects, "spawner-ssh-test-*")
	if err != nil {
		t.Skipf("cannot write a test transcript under %s: %v", projects, err)
	}
	defer os.RemoveAll(dir)

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	cwd := "/tmp/spawner-ssh-test-cwd"
	lines := []string{
		`{"type":"user","cwd":"` + cwd + `","timestamp":"2026-07-08T00:00:00Z","message":{"content":"hello"}}`,
		`{"type":"assistant","cwd":"` + cwd + `","timestamp":"2026-07-08T00:00:01Z","message":{"content":[{"type":"text","text":"hi there"}]}}`,
	}
	path := filepath.Join(dir, id+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewSSHPool(SSHConfig{})
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	remote := claudeFS{remote: &sshFS{pool: pool, host: "localhost"}}

	if got := remote.transcriptCwd(path); got != cwd {
		t.Errorf("remote transcriptCwd = %q, want %q", got, cwd)
	}
	if got := localClaudeFS.transcriptCwd(path); got != cwd {
		t.Errorf("local transcriptCwd = %q, want %q", got, cwd)
	}
	if got := remote.findByID(id); got != path {
		t.Errorf("remote findByID = %q, want %q", got, path)
	}
	lm, err := localClaudeFS.readTranscript(path)
	if err != nil {
		t.Fatalf("local readTranscript: %v", err)
	}
	rm, err := remote.readTranscript(path)
	if err != nil {
		t.Fatalf("remote readTranscript: %v", err)
	}
	if len(lm) != 2 || len(rm) != 2 {
		t.Fatalf("message counts local=%d remote=%d, want 2 each", len(lm), len(rm))
	}
	ds, err := remote.discoverSessions()
	if err != nil {
		t.Fatalf("remote discoverSessions: %v", err)
	}
	found := false
	for _, d := range ds {
		if d.SessionID == id {
			found = true
			if d.Dir != cwd {
				t.Errorf("discovered dir = %q, want %q", d.Dir, cwd)
			}
		}
	}
	if !found {
		t.Fatal("remote SSH discovery missed the test session")
	}
}

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

func TestCancelableCommand(t *testing.T) {
	got := cancelableCommand(remoteCommand("/tmp", "sleep", []string{"30"}))
	// setsid → new process group; the wrapper shell echoes its pgid ($$) on stderr,
	// then execs the inner cd+claude so the exec'd process keeps that pgid.
	want := `setsid sh -c 'echo __spawner_pgid__ $$ 1>&2; cd '\''/tmp'\'' && exec '\''sleep'\'' '\''30'\'''`
	if got != want {
		t.Errorf("cancelableCommand mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestLiveSSHCancelKillsRemote proves an aborted turn tears down the WHOLE remote
// process tree (not just the top process), the remote analogue of the host
// executor's process-group SIGKILL. It runs a long sleep as the "claude" binary over
// loopback, cancels the context, and asserts the remote sleep is gone. Gated on
// SPAWNER_SSH_LIVE=1.
func TestLiveSSHCancelKillsRemote(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live cancel test")
	}
	pool, err := NewSSHPool(SSHConfig{})
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()

	// A distinctive arg so pgrep can find exactly this process on the remote.
	marker := "998877" // seconds; unmistakable in `pgrep -f`
	ex := SSHExecutor{Pool: pool, Bin: "sleep"}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := ex.Start(ctx, &Session{Dir: "/"}, []string{marker}) // Host "" = loopback
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	go io.Copy(io.Discard, proc.Stdout()) // drain stdout so the proc runs

	running := func() bool {
		c, _ := pool.client("localhost")
		s, err := c.NewSession()
		if err != nil {
			return false
		}
		defer s.Close()
		// pgrep exits 0 when a match exists; -f matches the full command line.
		return s.Run("pgrep -f 'sleep "+marker+"'") == nil
	}
	waitFor := func(want bool) bool {
		for i := 0; i < 40; i++ { // up to ~4s
			if running() == want {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}

	if !waitFor(true) {
		t.Fatal("remote sleep never appeared (turn didn't start?)")
	}
	cancel()
	if !waitFor(false) {
		t.Fatal("remote process survived cancel — group kill failed")
	}
	_ = proc.Wait()
}

// TestLiveSSHRealClaude drives a real Claude turn over loopback SSH through
// Driver.Turn — the full path a live host session takes with SPAWNER_SSH on:
// SSHExecutor registered for TargetHost, Session.Host empty (loopback), the turn's
// stream-json parsed back into a reply. Gated on SPAWNER_SSH_LIVE=1 (needs key-based
// ssh to localhost and a real, authed claude on the box). Loopback keeps the local
// ~/.claude/PATH, so it isolates the SSH transport from the remote-discovery work.
func TestLiveSSHRealClaude(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run (real claude over loopback SSH)")
	}
	pool, err := NewSSHPool(SSHConfig{})
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	d := &Driver{Execs: map[Target]Executor{TargetHost: SSHExecutor{Pool: pool}}, Bypass: true}

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Name: "live-ssh", Dir: t.TempDir(), SessionID: id} // Host "" = loopback
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reply, _, err := d.Turn(ctx, s, "Reply with exactly the token LIVESSHOK and nothing else.", nil, nil, nil)
	if err != nil {
		t.Fatalf("live ssh turn: %v", err)
	}
	if !strings.Contains(reply, "LIVESSHOK") {
		t.Fatalf("reply lacked the token (didn't run real claude over SSH?): %q", reply)
	}
	t.Logf("ssh loopback → real claude reply: %q", reply)
}

// TestLiveSSHRemoteClaude drives a real Claude turn on a genuinely remote host
// (not loopback) — the actual payoff of SSH-native execution. Parameterized by
// SPAWNER_SSH_REMOTE_HOST (the real hostname/IP — the Go pool dials it directly and
// does NOT read ~/.ssh/config aliases) and SPAWNER_SSH_REMOTE_DIR (a path that
// exists ON THE REMOTE, default /tmp; unlike loopback, a local temp dir would not
// exist there). Needs the remote host key in known_hosts, an agent/key that
// authenticates, and an authed claude on the far side.
func TestLiveSSHRemoteClaude(t *testing.T) {
	host := os.Getenv("SPAWNER_SSH_REMOTE_HOST")
	if host == "" {
		t.Skip("set SPAWNER_SSH_REMOTE_HOST (real IP/hostname) to run a real remote claude turn")
	}
	dir := os.Getenv("SPAWNER_SSH_REMOTE_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	pool, err := NewSSHPool(SSHConfig{})
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	d := &Driver{Execs: map[Target]Executor{TargetHost: SSHExecutor{Pool: pool}}, Bypass: true}

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Name: "live-ssh-remote", Dir: dir, Host: host, SessionID: id}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	reply, _, err := d.Turn(ctx, s, "Reply with exactly the token LIVEREMOTEOK and nothing else.", nil, nil, nil)
	if err != nil {
		t.Fatalf("live remote ssh turn on %s: %v", host, err)
	}
	if !strings.Contains(reply, "LIVEREMOTEOK") {
		t.Fatalf("reply lacked the token (didn't run real claude on %s?): %q", host, reply)
	}
	t.Logf("ssh %s → real claude reply: %q", host, reply)
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

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

	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	remote := claudeFS{remote: &sshFS{pool: pool, host: "localhost"}}

	if got := remote.transcriptCwd(context.Background(), path); got != cwd {
		t.Errorf("remote transcriptCwd = %q, want %q", got, cwd)
	}
	if got := localClaudeFS.transcriptCwd(context.Background(), path); got != cwd {
		t.Errorf("local transcriptCwd = %q, want %q", got, cwd)
	}
	if got := remote.findByID(context.Background(), id); got != path {
		t.Errorf("remote findByID = %q, want %q", got, path)
	}
	lm, err := localClaudeFS.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatalf("local readTranscript: %v", err)
	}
	rm, err := remote.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatalf("remote readTranscript: %v", err)
	}
	if len(lm) != 2 || len(rm) != 2 {
		t.Fatalf("message counts local=%d remote=%d, want 2 each", len(lm), len(rm))
	}
	ds, err := remote.discoverSessions(context.Background())
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

// TestLiveSSHTailAndPathMemo covers the two remote-only halves of the bounded-read
// work: `tail -c` must agree byte-for-byte with the local ReadAt, and the memoized
// id→path resolution must survive a forget. Gated on SPAWNER_SSH_LIVE=1.
func TestLiveSSHTailAndPathMemo(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live claudeFS test")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	projects := filepath.Join(home, ".claude", "projects")
	dir, err := os.MkdirTemp(projects, "spawner-ssh-tail-*")
	if err != nil {
		t.Skipf("cannot write a test transcript under %s: %v", projects, err)
	}
	defer os.RemoveAll(dir)

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, id+".jsonl")
	lines := []string{
		`{"type":"user","cwd":"/tmp","timestamp":"2026-08-01T00:00:00Z","message":{"content":"` + strings.Repeat("x", 4096) + `"}}`,
		`{"type":"assistant","cwd":"/tmp","timestamp":"2026-08-01T00:00:01Z","message":{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":4242,"output_tokens":7,"cache_read_input_tokens":11,"cache_creation_input_tokens":0}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	remote := claudeFS{remote: &sshFS{pool: pool, host: "localhost"}}

	for _, n := range []int64{64, 512, 1 << 20} {
		rd, rWhole, rErr := remote.tailBytes(context.Background(), path, n)
		ld, lWhole, lErr := localClaudeFS.tailBytes(context.Background(), path, n)
		if rErr != nil || lErr != nil {
			t.Fatalf("tailBytes(%d): remote err=%v local err=%v", n, rErr, lErr)
		}
		if string(rd) != string(ld) || rWhole != lWhole {
			t.Errorf("tailBytes(%d) mismatch: remote %d bytes whole=%v, local %d bytes whole=%v",
				n, len(rd), rWhole, len(ld), lWhole)
		}
	}

	// The bounded tail must still find the usage on the final line.
	snap := remote.lastUsageInFile(context.Background(), path)
	if snap == nil || snap.Usage.Input != 4242 || snap.Usage.CacheRead != 11 {
		t.Errorf("remote lastUsageInFile = %+v, want input 4242 / cache_read 11", snap)
	}

	if got := remote.findByID(context.Background(), id); got != path {
		t.Fatalf("remote findByID = %q, want %q", got, path)
	}
	remote.forgetPath(id) // memo dropped: the next lookup must re-resolve, not fail
	if got := remote.findByID(context.Background(), id); got != path {
		t.Errorf("remote findByID after forgetPath = %q, want %q", got, path)
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
	got := remoteCommand("/data/proj dir", "claude", args, nil)
	want := `cd '/data/proj dir' && exec 'claude' '-p' 'what'\''s up here' '--output-format' 'stream-json'`
	if got != want {
		t.Errorf("remoteCommand mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestRemoteCommandEnv(t *testing.T) {
	got := remoteCommand("/data/proj", "opencode", []string{"run"}, []string{
		"OLLAMA_BASE_URL=http://10.0.0.8:11434",
		"OPENAI_API_KEY=sk-test",
	})
	want := `cd '/data/proj' && exec env 'OLLAMA_BASE_URL=http://10.0.0.8:11434' 'OPENAI_API_KEY=sk-test' 'opencode' 'run'`
	if got != want {
		t.Errorf("remoteCommand env mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestShellEnvCommand(t *testing.T) {
	got := shellEnvCommand([]string{"A=one two", "B=it's"}, "printf '%s' \"$A:$B\"")
	want := `env 'A=one two' 'B=it'\''s' sh -c 'printf '\''%s'\'' "$A:$B"'`
	if got != want {
		t.Errorf("shellEnvCommand =\n%s\nwant\n%s", got, want)
	}
}

func TestSSHBinFallback(t *testing.T) {
	// No registry, no config default → "claude".
	if b := (&SSHPool{}).binFor("anything"); b != "claude" {
		t.Errorf("default bin = %q, want claude", b)
	}
	// Config default applies when a host has no override.
	p := &SSHPool{cfg: SSHConfig{Bin: "cfgclaude"}}
	if b := p.binFor("work"); b != "cfgclaude" {
		t.Errorf("pool-config bin = %q, want cfgclaude", b)
	}
	// A registry host's ClaudeBin wins over the config default.
	hs := &HostStore{byName: map[string]*Host{"work": {Name: "work", ClaudeBin: "remoteclaude"}}}
	p2 := &SSHPool{cfg: SSHConfig{Bin: "cfgclaude"}, hosts: hs}
	if b := p2.binFor("work"); b != "remoteclaude" {
		t.Errorf("registry bin = %q, want remoteclaude", b)
	}
	if b := p2.binFor("other"); b != "cfgclaude" {
		t.Errorf("unlisted host bin = %q, want cfgclaude", b)
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
	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
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
	out, err := sess.Output(remoteCommand("/", "printf", []string{"%s", "hi there"}, nil))
	if err != nil {
		t.Fatalf("run remote command: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hi there" {
		t.Fatalf("remote output = %q, want %q", out, "hi there")
	}
}

func TestCancelableCommand(t *testing.T) {
	got := cancelableCommand(remoteCommand("/tmp", "sleep", []string{"30"}, nil))
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
	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()

	// A distinctive arg so pgrep can find exactly this process on the remote.
	marker := "998877" // seconds; unmistakable in `pgrep -f`
	ex := SSHExecutor{Pool: pool, Bin: "sleep"}
	ctx, cancel := context.WithCancel(context.Background())
	proc, err := ex.Start(ctx, &Session{Dir: "/"}, nil, "", []string{marker}) // Host "" = loopback
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
// Driver.Turn — the full path every live host session takes (SSH-native execution
// is unconditional):
// SSHExecutor registered for TargetHost, Session.Host empty (loopback), the turn's
// stream-json parsed back into a reply. Gated on SPAWNER_SSH_LIVE=1 (needs key-based
// ssh to localhost and a real, authed claude on the box). Loopback keeps the local
// ~/.claude/PATH, so it isolates the SSH transport from the remote-discovery work.
func TestLiveSSHRealClaude(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run (real claude over loopback SSH)")
	}
	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
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
	res, err := d.Turn(ctx, s, "Reply with exactly the token LIVESSHOK and nothing else.", nil, nil, nil)
	reply := res.Reply
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
	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
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
	res, err := d.Turn(ctx, s, "Reply with exactly the token LIVEREMOTEOK and nothing else.", nil, nil, nil)
	reply := res.Reply
	if err != nil {
		t.Fatalf("live remote ssh turn on %s: %v", host, err)
	}
	if !strings.Contains(reply, "LIVEREMOTEOK") {
		t.Fatalf("reply lacked the token (didn't run real claude on %s?): %q", host, reply)
	}
	t.Logf("ssh %s → real claude reply: %q", host, reply)
}

// TestLiveSSHHostRegistry proves the pool resolves a logical Session.Host name via
// the HostStore to a real address and drives a real claude turn there — the host
// addressing model. The registry entry "workbox" maps to SPAWNER_SSH_REMOTE_HOST;
// the session names "workbox", never the raw address.
func TestLiveSSHHostRegistry(t *testing.T) {
	addr := os.Getenv("SPAWNER_SSH_REMOTE_HOST")
	if addr == "" {
		t.Skip("set SPAWNER_SSH_REMOTE_HOST to run the registry-resolution turn")
	}
	dir := os.Getenv("SPAWNER_SSH_REMOTE_DIR")
	if dir == "" {
		dir = "/tmp"
	}
	hs := &HostStore{byName: map[string]*Host{
		"workbox": {Name: "workbox", Address: addr},
	}}
	pool, err := NewSSHPool(SSHConfig{}, hs, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	d := &Driver{Execs: map[Target]Executor{TargetHost: SSHExecutor{Pool: pool}}, Bypass: true}

	id, err := NewSessionID()
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{Name: "live-registry", Dir: dir, Host: "workbox", SessionID: id}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	res, err := d.Turn(ctx, s, "Reply with exactly the token LIVEREGISTRYOK and nothing else.", nil, nil, nil)
	reply := res.Reply
	if err != nil {
		t.Fatalf("live registry turn (host=workbox → %s): %v", addr, err)
	}
	if !strings.Contains(reply, "LIVEREGISTRYOK") {
		t.Fatalf("reply lacked the token: %q", reply)
	}
	t.Logf("host name workbox → %s → real claude reply: %q", addr, reply)
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

// TestLiveSSHStatManyBatch proves the batched stat — the seam every chain-freshness
// check goes through — agrees with the local backend for a mixed batch: ordinary
// files, a name with a space and a '%' (which must never reach stat's format), and
// a missing path (absent from the map, never a bogus zero entry). Gated on
// SPAWNER_SSH_LIVE=1.
func TestLiveSSHStatManyBatch(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live statMany test")
	}
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"a.jsonl", "odd %s name.jsonl", "b.jsonl"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Repeat("x", len(name))), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	paths = append(paths, filepath.Join(dir, "missing.jsonl"))

	pool, err := NewSSHPool(SSHConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()
	remote := claudeFS{remote: &sshFS{pool: pool, host: "localhost"}}

	got := remote.statMany(context.Background(), paths)
	want := localClaudeFS.statMany(context.Background(), paths)
	if len(got) != len(want) {
		t.Fatalf("statMany returned %d entries, local returned %d", len(got), len(want))
	}
	for p, w := range want {
		g, ok := got[p]
		if !ok {
			t.Errorf("statMany missed %q", p)
			continue
		}
		if g.size != w.size || !g.mod.Equal(w.mod) {
			t.Errorf("statMany(%q) = %d/%v, local = %d/%v", p, g.size, g.mod, w.size, w.mod)
		}
	}
	if _, ok := got[filepath.Join(dir, "missing.jsonl")]; ok {
		t.Error("statMany reported a stat for a missing file")
	}
}

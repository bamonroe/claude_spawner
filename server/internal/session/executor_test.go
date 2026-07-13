package session

import (
	"context"
	"io"
	"strings"
	"testing"
)

// fakeExecutor records the last launch and replays a canned stdout stream.
type fakeExecutor struct {
	id       string // written into the result text so we can tell executors apart
	gotDir   string
	gotArgs  []string
	launched bool
}

func (f *fakeExecutor) Start(ctx context.Context, s *Session, bin string, args []string) (Proc, error) {
	f.launched, f.gotDir, f.gotArgs = true, s.Dir, args
	line := `{"type":"result","subtype":"success","result":"` + f.id + `"}` + "\n"
	return &fakeProc{r: strings.NewReader(line)}, nil
}

type fakeProc struct{ r io.Reader }

func (p *fakeProc) Stdout() io.Reader { return p.r }
func (p *fakeProc) Wait() error       { return nil }

func TestSandboxCreateArgs(t *testing.T) {
	s := SandboxExecutor{
		Runtime:   "podman",
		Image:     "spawner-sandbox:latest",
		Mounts:    []string{"/home/bam/.claude:/root/.claude"},
		RunArgs:   []string{"--userns=keep-id", "--network=none"},
		HomeMount: "/home/bam",
	}
	got := strings.Join(s.createArgs("spawner-abc123", "/work/proj"), " ")

	for _, want := range []string{
		"run -d --name spawner-abc123",          // detached, session-named container
		"-w /work/proj",                         // container workdir = session dir
		"-v /work/proj:/work/proj",              // same-path mount (transcript encoding)
		"-v /home/bam:/home/bam",                // whole host home, read-write
		"-v /home/bam/.claude:/root/.claude",    // shared claude state
		"--userns=keep-id --network=none",       // extra run flags
		"spawner-sandbox:latest sleep infinity", // image, then keep-alive command
	} {
		if !strings.Contains(got, want) {
			t.Errorf("createArgs missing %q\n got: %s", want, got)
		}
	}
	if strings.Index(got, "--network=none") > strings.Index(got, "spawner-sandbox:latest") {
		t.Errorf("run flags must come before the image: %s", got)
	}
}

func TestSandboxExecArgs(t *testing.T) {
	s := SandboxExecutor{Runtime: "podman", Image: "img"}
	got := strings.Join(s.execArgs("spawner-abc123", "/work/proj", "claude", []string{"-p", "hi", "--resume", "sid"}), " ")
	// exec into the running container, workdir = session dir, default claude bin.
	want := "exec -i -w /work/proj spawner-abc123 claude -p hi --resume sid"
	if got != want {
		t.Errorf("execArgs = %q, want %q", got, want)
	}
}

// The SSH-native sandbox runs `podman exec claude` on the host: every token must be
// single-quoted so a prompt with spaces/quotes reaches the remote shell verbatim, and
// the line is prefixed with exec so podman inherits the cancelable wrapper's pgid.
func TestSandboxSSHInnerCommandQuoted(t *testing.T) {
	s := SandboxExecutor{Runtime: "podman", Image: "img"}
	args := s.execArgs("spawner-abc123", "/work/proj", "claude", []string{"-p", "fix the 'bug' now", "--output-format", "stream-json"})
	inner := "exec " + shellJoinCmd(s.Runtime, args)
	want := `exec 'podman' 'exec' '-i' '-w' '/work/proj' 'spawner-abc123' 'claude' '-p' 'fix the '\''bug'\'' now' '--output-format' 'stream-json'`
	if inner != want {
		t.Errorf("inner =\n %q\nwant\n %q", inner, want)
	}
}

// A sandbox executor with no explicit Host runs the runtime on the local box; an
// explicit Host is honored.
func TestSandboxHostDefault(t *testing.T) {
	if got := (SandboxExecutor{}).host(); got != LocalHost {
		t.Errorf("default host = %q, want %q", got, LocalHost)
	}
	if got := (SandboxExecutor{Host: "work"}).host(); got != "work" {
		t.Errorf("host = %q, want work", got)
	}
}

// fakeReaper is a sandbox executor that lists a fixed container set and records
// removals, for testing orphan reconciliation.
type fakeReaper struct {
	all     []string
	removed []string
}

func (f *fakeReaper) Start(context.Context, *Session, string, []string) (Proc, error) {
	return nil, nil
}
func (f *fakeReaper) Ensure(context.Context, *Session) error { return nil }
func (f *fakeReaper) List(context.Context) ([]string, error) { return f.all, nil }
func (f *fakeReaper) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

func TestReconcileContainersRemovesOrphans(t *testing.T) {
	reaper := &fakeReaper{all: []string{"spawner-keep", "spawner-orphan1", "spawner-orphan2"}}
	d := &Driver{Execs: map[Target]Executor{TargetHost: HostExecutor{}, TargetSandbox: reaper}}

	removed, err := d.ReconcileContainers(context.Background(), map[string]bool{"spawner-keep": true})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"spawner-orphan1", "spawner-orphan2"}
	if strings.Join(removed, ",") != strings.Join(want, ",") {
		t.Errorf("removed = %v, want %v", removed, want)
	}
	// The live session's container must be left alone.
	for _, n := range reaper.removed {
		if n == "spawner-keep" {
			t.Errorf("reconcile removed a live container %q", n)
		}
	}
}

func TestSandboxPrefixIsolation(t *testing.T) {
	// Default (production) executor lists/reaps under the shared prefix.
	if got := (SandboxExecutor{}).prefix(); got != containerPrefix {
		t.Errorf("default prefix = %q, want %q", got, containerPrefix)
	}
	// A test executor's prefix overrides it, so its List can only ever match its
	// own containers — never a real session's under the production prefix.
	const testPrefix = "spawner-sbxtest-abc-"
	if got := (SandboxExecutor{Prefix: testPrefix}).prefix(); got != testPrefix {
		t.Errorf("override prefix = %q, want %q", got, testPrefix)
	}
	// The two namespaces must not overlap: the production filter must not match a
	// test container name, nor vice versa (podman --filter name= is a substring
	// match), or a test reconcile could still sweep a live session's container.
	cn, err := NewContainerNameWithPrefix(testPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cn, testPrefix) {
		t.Errorf("name %q lacks prefix %q", cn, testPrefix)
	}
	if strings.Contains(cn, containerPrefix) {
		t.Errorf("test container %q contains the production prefix %q — reconcile would cross namespaces", cn, containerPrefix)
	}
}

func TestSandboxCreateArgsUsesProfile(t *testing.T) {
	s := SandboxExecutor{
		Runtime:   "podman",
		Image:     "base-image",
		Mounts:    []string{"/base:/base:ro"},
		RunArgs:   []string{"--network=none"},
		HomeMount: "/home/bam",
	}
	p := &ExecProfile{
		Name:    "open",
		Image:   "profile-image",
		Mounts:  []string{"/home/bam/src:/src:rw"},
		Creds:   []string{"/secrets/opencode.json:/root/.local/share/opencode/auth.json:ro"},
		Env:     map[string]string{"OLLAMA_BASE_URL": "http://10.0.0.8:11434", "OPENAI_API_KEY": "abc"},
		RunArgs: []string{"--dns", "100.100.100.100"},
	}
	got := strings.Join(s.createArgsFor("spawner-abc123", "/work/proj", p), " ")

	for _, want := range []string{
		"-v /home/bam/src:/src:rw",
		"-v /secrets/opencode.json:/root/.local/share/opencode/auth.json:ro",
		"-e OLLAMA_BASE_URL=http://10.0.0.8:11434",
		"-e OPENAI_API_KEY=abc",
		"--dns 100.100.100.100",
		"profile-image sleep infinity",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("profile createArgs missing %q\n got: %s", want, got)
		}
	}
	for _, old := range []string{"/base:/base:ro", "--network=none", "base-image sleep infinity"} {
		if strings.Contains(got, old) {
			t.Errorf("profile createArgs should override executor value %q\n got: %s", old, got)
		}
	}
}

func TestReconcileContainersNoReaper(t *testing.T) {
	// Host-only driver: nothing to reconcile, no error.
	d := &Driver{Execs: map[Target]Executor{TargetHost: HostExecutor{}}}
	removed, err := d.ReconcileContainers(context.Background(), nil)
	if err != nil || removed != nil {
		t.Errorf("got (%v, %v), want (nil, nil)", removed, err)
	}
}

func TestExecutorSelectByTarget(t *testing.T) {
	host := &fakeExecutor{id: "host"}
	sandbox := &fakeExecutor{id: "sandbox"}
	d := &Driver{Execs: map[Target]Executor{TargetHost: host, TargetSandbox: sandbox}}

	cases := []struct {
		target Target
		want   string
	}{
		{"", "host"},         // empty falls back to host
		{TargetHost, "host"}, //
		{TargetSandbox, "sandbox"},
		{"bogus", "host"}, // unknown target falls back to host
	}
	for _, tc := range cases {
		s := &Session{Name: "s", Dir: "/tmp/x", SessionID: "id", Target: tc.target}
		got, _, err := d.Turn(context.Background(), s, "hi", nil, nil, nil)
		if err != nil {
			t.Fatalf("target %q: Turn: %v", tc.target, err)
		}
		if got != tc.want {
			t.Errorf("target %q routed to %q, want %q", tc.target, got, tc.want)
		}
	}
}

func TestTurnPassesDirAndArgs(t *testing.T) {
	host := &fakeExecutor{id: "ok"}
	d := &Driver{Execs: map[Target]Executor{TargetHost: host}, Bypass: true}

	// First turn: --session-id, plus bypass, run in the session's Dir.
	s := &Session{Name: "s", Dir: "/work/proj", SessionID: "sid-1"}
	if _, _, err := d.Turn(context.Background(), s, "hello", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if host.gotDir != "/work/proj" {
		t.Errorf("dir = %q, want /work/proj", host.gotDir)
	}
	joined := strings.Join(host.gotArgs, " ")
	for _, want := range []string{"-p hello", "--session-id sid-1", "--dangerously-skip-permissions"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q missing %q", joined, want)
		}
	}
	if strings.Contains(joined, "--resume") {
		t.Errorf("first turn should not --resume: %q", joined)
	}

	// s.Started flipped true → next turn resumes instead of creating.
	if _, _, err := d.Turn(context.Background(), s, "again", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(host.gotArgs, " ")
	if !strings.Contains(joined, "--resume sid-1") || strings.Contains(joined, "--session-id") {
		t.Errorf("second turn should --resume, not --session-id: %q", joined)
	}
}

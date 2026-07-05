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

func (f *fakeExecutor) Start(ctx context.Context, dir string, args []string) (Proc, error) {
	f.launched, f.gotDir, f.gotArgs = true, dir, args
	line := `{"type":"result","subtype":"success","result":"` + f.id + `"}` + "\n"
	return &fakeProc{r: strings.NewReader(line)}, nil
}

type fakeProc struct{ r io.Reader }

func (p *fakeProc) Stdout() io.Reader { return p.r }
func (p *fakeProc) Wait() error       { return nil }

func TestSandboxRunArgs(t *testing.T) {
	s := SandboxExecutor{
		Runtime: "podman",
		Image:   "spawner-sandbox:latest",
		Mounts:  []string{"/home/bam/.claude:/root/.claude"},
		RunArgs: []string{"--userns=keep-id", "--network=none"},
	}
	got := strings.Join(s.runArgs("/work/proj", []string{"-p", "hi", "--session-id", "sid"}), " ")

	for _, want := range []string{
		"run --rm -i",                         // disposable, stdin open
		"-w /work/proj",                       // container workdir = session dir
		"-v /work/proj:/work/proj",            // same-path mount (transcript encoding)
		"-v /home/bam/.claude:/root/.claude",  // shared claude state
		"--userns=keep-id --network=none",     // extra run flags
		"spawner-sandbox:latest claude -p hi", // image, default bin, then claude args
		"--session-id sid",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("runArgs missing %q\n got: %s", want, got)
		}
	}
	// The image must precede the claude command, and run-flags must precede the image.
	if strings.Index(got, "--network=none") > strings.Index(got, "spawner-sandbox") {
		t.Errorf("run flags must come before the image: %s", got)
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

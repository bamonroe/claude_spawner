package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/broker"
)

// Target names where a session's turns run. It is a durable per-session property
// (Session.Target) so host-vs-sandbox is chosen once, at spawn time. The empty
// string means TargetHost, so records written before this field existed — and the
// account-global Usage call — resolve to the host executor. See the "containerized
// server + per-session execution target" section in docs/architecture.md.
type Target string

const (
	// TargetHost runs the turn as a direct child process on the host (today's
	// behavior). The session's Dir is a real host path and edits land on host files.
	TargetHost Target = "host"
	// TargetSandbox runs the turn inside an isolated container (root inside the
	// sandbox, disposable filesystem) via a rootless container runtime.
	TargetSandbox Target = "sandbox"
)

// Executor launches one `claude` invocation and exposes its stdout stream and
// lifecycle. It is the seam that lets a turn run on the host (direct exec), inside
// a container sandbox, or via a host-side broker without Driver.Turn knowing which
// — Turn builds the claude args and parses the stream; the Executor only decides
// how the process is spawned. Implementations must run the process in the given
// working directory and kill it (and its children) when ctx is cancelled.
type Executor interface {
	// Start launches `claude <args...>` for session s (it reads s.Dir, and for the
	// sandbox target s.Container) and returns a running Proc. The caller reads
	// Proc.Stdout to EOF, then calls Proc.Wait.
	Start(ctx context.Context, s *Session, args []string) (Proc, error)
}

// containerPrefix names every sandbox container this server manages, so they can
// be listed for orphan reconciliation and told apart from unrelated containers.
const containerPrefix = "spawner-"

// SandboxLifecycle is implemented by executors that own a per-session container
// bound to the session's lifetime: created at spawn (Ensure), reused by every
// turn, and destroyed on delete (Remove). Driver.EnsureContainer /
// RemoveContainer call these at the spawn/delete hooks.
type SandboxLifecycle interface {
	// Ensure makes sure the named container exists and is running for a session
	// rooted at dir, creating it if absent. Idempotent.
	Ensure(ctx context.Context, name, dir string) error
	// Remove force-deletes the named container (no error if it's already gone).
	Remove(ctx context.Context, name string) error
}

// SandboxReaper lists this server's sandbox containers so orphans (containers
// whose session was deleted while the server was down) can be swept at startup.
type SandboxReaper interface {
	// List returns the names of all sandbox containers this server manages,
	// running or stopped.
	List(ctx context.Context) ([]string, error)
	// Remove force-deletes the named container.
	Remove(ctx context.Context, name string) error
}

// Proc is a launched claude process. The caller reads Stdout to EOF (the
// stream-json events) and then calls Wait exactly once.
type Proc interface {
	// Stdout is the process's stdout — the stream-json event stream.
	Stdout() io.Reader
	// Wait blocks until the process exits and returns its exit error, if any.
	Wait() error
}

// HostExecutor runs claude as a direct child process on the host. This is the
// default and reproduces the original inline exec: its own process group with a
// group-kill on ctx cancel, so an aborted turn takes claude AND any tool child it
// spawned (a build, a sleep) down with it, not just the top-level process.
type HostExecutor struct {
	// Bin is the claude binary (path or name resolved via PATH).
	Bin string
}

func (h HostExecutor) Start(ctx context.Context, s *Session, args []string) (Proc, error) {
	return startProc(ctx, h.Bin, args, s.Dir, "start claude")
}

// SandboxExecutor runs a session's turns inside a persistent, isolated container
// via a rootless runtime (Podman rootless, or rootless Docker) — so the sandbox
// gets root INSIDE itself and a disposable filesystem, without launching it
// requiring host root. The container's lifetime is bound to the session: it's
// created at spawn (Ensure), each turn runs via `exec` into it, and it's destroyed
// when the session is deleted (Remove). So packages installed and services started
// in one turn persist to the next — a real environment, not a fresh box per turn.
// The session's Dir is bind-mounted at the same path and used as the workdir, so
// file edits land there AND claude's on-disk transcript is keyed by the same
// absolute path the host uses (keeping history/discovery working when the host
// ~/.claude state is shared via Mounts).
type SandboxExecutor struct {
	// Runtime is the container CLI (e.g. "podman" for rootless, or "docker").
	Runtime string
	// Image is the container image carrying claude + the project toolchain.
	Image string
	// Bin is the claude binary inside the image (default "claude").
	Bin string
	// Mounts are extra volume specs ("host:container[:opts]") passed as -v, e.g.
	// sharing "$HOME/.claude" so in-sandbox transcripts stay discoverable by the
	// host, or mounting auth read-only.
	Mounts []string
	// RunArgs are extra `run` flags inserted before the image, e.g.
	// "--userns=keep-id" (rootless uid mapping) or "--network=none" (offline).
	RunArgs []string
}

func (s SandboxExecutor) bin() string {
	if s.Bin == "" {
		return "claude"
	}
	return s.Bin
}

// Start runs one turn by exec'ing claude inside the session's persistent
// container, (re)creating the container first if it isn't running (so a turn
// survives a server restart or a manually-removed container).
func (s SandboxExecutor) Start(ctx context.Context, sess *Session, claudeArgs []string) (Proc, error) {
	if sess.Container == "" {
		return nil, fmt.Errorf("sandbox session %q has no container name", sess.Name)
	}
	if err := s.Ensure(ctx, sess.Container, sess.Dir); err != nil {
		return nil, err
	}
	// `exec` is itself a local child process; killing its group on abort stops the
	// exec client (and the runtime signals the in-container process).
	return startProc(ctx, s.Runtime, s.execArgs(sess.Container, sess.Dir, claudeArgs), "", "start sandbox turn")
}

// Ensure creates and starts the session's long-lived container if it isn't
// already running. The container just idles (`sleep infinity`); turns run via
// exec. A stale stopped container of the same name is removed first.
func (s SandboxExecutor) Ensure(ctx context.Context, name, dir string) error {
	if s.running(ctx, name) {
		return nil
	}
	_ = s.Remove(ctx, name) // clear a stopped leftover so the name is free
	if out, err := runCLI(ctx, s.Runtime, s.createArgs(name, dir)); err != nil {
		return fmt.Errorf("create sandbox %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Remove force-deletes the session's container. A missing container is not an
// error (delete is idempotent).
func (s SandboxExecutor) Remove(ctx context.Context, name string) error {
	if _, err := runCLI(ctx, s.Runtime, []string{"rm", "-f", name}); err != nil {
		return err
	}
	return nil
}

// List returns the names of all sandbox containers this server manages (running
// or stopped), matched by the shared name prefix.
func (s SandboxExecutor) List(ctx context.Context) ([]string, error) {
	out, err := runCLI(ctx, s.Runtime, []string{"ps", "-a", "--filter", "name=" + containerPrefix, "--format", "{{.Names}}"})
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w: %s", err, strings.TrimSpace(out))
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// running reports whether the named container exists and is currently running.
func (s SandboxExecutor) running(ctx context.Context, name string) bool {
	out, err := runCLI(ctx, s.Runtime, []string{"inspect", "-f", "{{.State.Running}}", name})
	return err == nil && strings.TrimSpace(out) == "true"
}

// createArgs builds the argv that starts the idle session container: run detached
// in the session dir (mounted same-path so the transcript's project encoding
// matches the host), plus extra mounts and run-flags, the image, and a keep-alive
// command. Split out for unit-testing without a runtime.
func (s SandboxExecutor) createArgs(name, dir string) []string {
	args := []string{"run", "-d", "--name", name, "-w", dir, "-v", dir + ":" + dir}
	for _, m := range s.Mounts {
		args = append(args, "-v", m)
	}
	args = append(args, s.RunArgs...)
	args = append(args, s.Image, "sleep", "infinity")
	return args
}

// execArgs builds the argv for one turn: exec claude in the session's workdir
// inside the already-running container.
func (s SandboxExecutor) execArgs(name, dir string, claudeArgs []string) []string {
	args := []string{"exec", "-i", "-w", dir, name, s.bin()}
	return append(args, claudeArgs...)
}

// runCLI runs a short runtime command (create/inspect/rm) to completion and
// returns its combined output. Used for lifecycle control, not for streaming a
// turn (that goes through startProc).
func runCLI(ctx context.Context, name string, args []string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// BrokerExecutor runs a turn on the host via the host-side broker daemon, for
// when the server itself is containerized: the server container stays
// unprivileged and can only ask the broker (running as the ordinary host user) to
// launch claude in a jailed directory. This is how a "host" session executes
// without the server holding host root. See internal/broker and
// docs/architecture.md.
type BrokerExecutor struct {
	// Socket is the path to the broker's Unix socket (bind-mounted into the
	// server container).
	Socket string
}

func (b BrokerExecutor) Start(ctx context.Context, s *Session, args []string) (Proc, error) {
	conn, err := net.Dial("unix", b.Socket)
	if err != nil {
		return nil, fmt.Errorf("dial broker: %w", err)
	}
	hdr, err := json.Marshal(broker.Request{Dir: s.Dir, Args: args})
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err := broker.WriteFrame(conn, broker.FrameHeader, hdr); err != nil {
		conn.Close()
		return nil, fmt.Errorf("broker header: %w", err)
	}
	p := &brokerProc{conn: conn}
	// Closing the socket is how we signal an abort to the broker (it kills the
	// turn's process group). Watch ctx and close on cancel; stop the watcher when
	// the turn ends normally so it doesn't leak.
	watchDone := make(chan struct{})
	var once sync.Once
	p.stop = func() { once.Do(func() { close(watchDone) }) }
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchDone:
		}
	}()
	return p, nil
}

// brokerProc de-frames the broker's reply: FrameStdout payloads are exposed as a
// plain stdout stream (Stdout returns the proc itself as an io.Reader); the
// terminal FrameExit ends the stream (EOF) and its status is surfaced by Wait.
type brokerProc struct {
	conn net.Conn
	stop func()

	buf  []byte       // leftover stdout payload from the last frame
	exit *broker.Exit // set when the exit frame arrives
	err  error        // terminal transport error
	done bool
}

func (p *brokerProc) Stdout() io.Reader { return p }

func (p *brokerProc) Read(dst []byte) (int, error) {
	for len(p.buf) == 0 {
		if p.done {
			if p.err != nil {
				return 0, p.err
			}
			return 0, io.EOF
		}
		typ, payload, err := broker.ReadFrame(p.conn)
		if err != nil {
			p.done, p.err = true, err
			return 0, err
		}
		switch typ {
		case broker.FrameStdout:
			p.buf = payload
		case broker.FrameExit:
			var e broker.Exit
			_ = json.Unmarshal(payload, &e)
			p.exit, p.done = &e, true
			return 0, io.EOF
		default:
			p.done = true
			p.err = fmt.Errorf("broker: unexpected frame %q", typ)
			return 0, p.err
		}
	}
	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

func (p *brokerProc) Wait() error {
	if p.stop != nil {
		p.stop()
	}
	p.conn.Close()
	switch {
	case p.exit == nil && p.err != nil:
		return fmt.Errorf("broker transport: %w", p.err)
	case p.exit == nil:
		return fmt.Errorf("broker: stream ended without an exit status")
	case p.exit.Err != "":
		return fmt.Errorf("broker: %s", p.exit.Err)
	case p.exit.Code != 0:
		return fmt.Errorf("status %d", p.exit.Code)
	}
	return nil
}

// startProc launches name+args as a local child process in its own process group
// and returns a Proc over its stdout. On ctx cancel the whole group is SIGKILLed
// so a turn's tool children (a build, a sleep, or a container client) die with it,
// not just the top-level process. dir sets the working directory when non-empty.
// startErrPrefix labels a launch failure.
func startProc(ctx context.Context, name string, args []string, dir, startErrPrefix string) (Proc, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%s: %w", startErrPrefix, err)
	}
	return &cmdProc{cmd: cmd, stdout: stdout}, nil
}

// cmdProc adapts an *exec.Cmd to Proc. Used by any executor that ultimately runs
// claude through the local process table (the host directly, or a container
// runtime CLI like `podman run`, which is itself just a child process here).
type cmdProc struct {
	cmd    *exec.Cmd
	stdout io.Reader
}

func (p *cmdProc) Stdout() io.Reader { return p.stdout }
func (p *cmdProc) Wait() error       { return p.cmd.Wait() }

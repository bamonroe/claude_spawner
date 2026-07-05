package session

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"syscall"
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
	// Start launches `claude <args...>` with working directory dir and returns a
	// running Proc. The caller reads Proc.Stdout to EOF, then calls Proc.Wait.
	Start(ctx context.Context, dir string, args []string) (Proc, error)
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

func (h HostExecutor) Start(ctx context.Context, dir string, args []string) (Proc, error) {
	return startProc(ctx, h.Bin, args, dir, "start claude")
}

// SandboxExecutor runs a turn inside an isolated container via a rootless
// container runtime (Podman rootless, or rootless Docker) — so the sandbox gets
// root INSIDE itself and a disposable filesystem, without launching it requiring
// host root. The session's Dir is bind-mounted at the same path and used as the
// container workdir, so file edits land there AND claude's on-disk transcript is
// keyed by the same absolute path the host uses (keeping history/discovery
// working when the host ~/.claude state is shared via Mounts).
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

func (s SandboxExecutor) Start(ctx context.Context, dir string, claudeArgs []string) (Proc, error) {
	// The runtime CLI is itself a local child process; killing its group on abort
	// stops the client (the container is reaped by --rm on exit).
	return startProc(ctx, s.Runtime, s.runArgs(dir, claudeArgs), "", "start sandbox")
}

// runArgs builds the container runtime's argv: run --rm -i in the session dir
// (mounted same-path so the transcript's project encoding matches the host), then
// extra mounts and run-flags, the image, and finally the claude command. Split
// out from Start so it's unit-testable without launching a container.
func (s SandboxExecutor) runArgs(dir string, claudeArgs []string) []string {
	bin := s.Bin
	if bin == "" {
		bin = "claude"
	}
	args := []string{"run", "--rm", "-i", "-w", dir, "-v", dir + ":" + dir}
	for _, m := range s.Mounts {
		args = append(args, "-v", m)
	}
	args = append(args, s.RunArgs...)
	args = append(args, s.Image, bin)
	return append(args, claudeArgs...)
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

package session

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
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

// LocalHost is the explicit host name for the loopback machine — the SSH-native
// path treats it as just another registered host (dialed over loopback SSH), not
// as a special implicit default. Every host-target session carries an explicit
// Session.Host; a session with no host is an error, never silently "localhost".
// So a deployment can drive purely remote hosts and never reach the local box.
const LocalHost = "localhost"

// Executor launches one `claude` invocation and exposes its stdout stream and
// lifecycle. It is the seam that lets a turn run on the host (direct exec) or
// inside a container sandbox without Driver.Turn knowing which — Turn builds the
// claude args and parses the stream; the Executor only decides how the process is
// spawned. Implementations must run the process in the given working directory and
// kill it (and its children) when ctx is cancelled.
type Executor interface {
	// Start launches `<bin> <args...>` for session s (it reads s.Dir, and for the
	// sandbox target s.Container) and returns a running Proc. bin is the backend's
	// command (e.g. "claude" or "codex"), resolved by the Driver from the session's
	// agent; an empty bin defers to the executor's own configured binary, so a
	// Claude session (bin "") keeps using each target's SPAWNER_*_CLAUDE_BIN. The
	// caller reads Proc.Stdout to EOF, then calls Proc.Wait.
	Start(ctx context.Context, s *Session, bin string, args []string) (Proc, error)
}

// containerPrefix names every sandbox SESSION container this server manages, so
// they can be listed for orphan reconciliation and told apart from unrelated
// containers. It is deliberately specific ("spawner-sbx-", not "spawner-") so the
// reconcile's name filter can't match infrastructure containers like the server's
// own container (e.g. "claude-spawner-server") and remove it as an orphan.
const containerPrefix = "spawner-sbx-"

// SandboxLifecycle is implemented by executors that own a per-session container
// bound to the session's lifetime: created at spawn (Ensure), reused by every
// turn, and destroyed on delete (Remove). Driver.EnsureContainer /
// RemoveContainer call these at the spawn/delete hooks.
type SandboxLifecycle interface {
	// Ensure makes sure the named container exists and is running for a session
	// rooted at dir, creating it if absent. Idempotent.
	Ensure(ctx context.Context, sess *Session) error
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

// HostExecutor runs claude as a direct child process on the host, in its own
// process group with a group-kill on ctx cancel. The production server never uses
// it — host turns always run over SSH (SSHExecutor) — but the unit tests do: it's
// the hermetic turn executor that forks a fake `claude` without needing a live
// sshd. NewDriver keeps it as the default so tests get a working host target.
type HostExecutor struct {
	// Bin is the claude binary (path or name resolved via PATH).
	Bin string
}

func (h HostExecutor) Start(ctx context.Context, s *Session, bin string, args []string) (Proc, error) {
	if bin == "" {
		bin = h.Bin
	}
	return startProcEnv(ctx, bin, args, s.Dir, "start turn", s.ResolvedProfile.envList())
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
//
// SSH-native mode: Pool is set in production, so the runtime CLI
// (create/exec/rm/inspect) runs over SSH on Host — a containerized, SSH-native
// server, which has no container runtime of its own, drives rootless podman on the
// host exactly the way it runs host turns there. All mount/dir paths are then HOST
// paths (the session Dir and Mounts already are, since sessions are created against
// the host filesystem), and the transcript is read back over SSH on Host. With Pool
// nil (tests only) it runs the runtime as local child processes.
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
	// HomeMount, when non-empty, is the host home directory bind-mounted
	// read-write at the same path inside every sandbox (e.g. "/home/bam" ->
	// "/home/bam"), so the user's whole home — dotfiles, ~/.claude, project
	// checkouts — is available and writable in the container the same way it is on
	// the host. Set from the server's $HOME. Skipped when empty or when it would
	// duplicate the session-dir mount.
	HomeMount string
	// RunArgs are extra `run` flags inserted before the image, e.g.
	// "--userns=keep-id" (rootless uid mapping) or "--network=none" (offline).
	RunArgs []string
	// Prefix namespaces this executor's containers for List/reconcile. Empty means
	// the production default (containerPrefix). Reconcile only ever sees — and so
	// can only ever remove — containers under this prefix, so a test executor set
	// to a unique prefix can never sweep another server's (or a real session's)
	// live containers. Production leaves this empty.
	Prefix string
	// Pool, when non-nil, routes every runtime command over SSH to Host instead of
	// running it as a local child process — the SSH-native path (see the type doc).
	Pool *SSHPool
	// Host is the registered host the runtime runs on when Pool is set (default
	// LocalHost — the machine the container is co-located with, reached over loopback
	// SSH). Ignored in local mode.
	Host string
}

// host is the SSH host the runtime runs on in SSH-native mode, defaulting to the
// local box.
func (s SandboxExecutor) host() string {
	if s.Host != "" {
		return s.Host
	}
	return LocalHost
}

// ctl runs a short runtime control command (create/inspect/rm/ps) to completion and
// returns its combined output. Local mode execs the runtime directly; SSH-native
// mode runs it on the host over the pool, folding stderr into stdout (2>&1) to match
// the local CombinedOutput so error messages survive.
func (s SandboxExecutor) ctl(ctx context.Context, args []string) (string, error) {
	if s.Pool != nil {
		out, err := s.Pool.Run(ctx, s.host(), shellJoinCmd(s.Runtime, args)+" 2>&1")
		return string(out), err
	}
	return runCLI(ctx, s.Runtime, args)
}

// prefix returns the container-name namespace this executor lists and reaps.
func (s SandboxExecutor) prefix() string {
	if s.Prefix != "" {
		return s.Prefix
	}
	return containerPrefix
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
func (s SandboxExecutor) Start(ctx context.Context, sess *Session, bin string, turnArgs []string) (Proc, error) {
	if sess.Container == "" {
		return nil, fmt.Errorf("sandbox session %q has no container name", sess.Name)
	}
	if err := s.Ensure(ctx, sess); err != nil {
		return nil, err
	}
	if bin == "" {
		bin = s.bin()
	}
	args := s.execArgs(sess.Container, sess.Dir, bin, turnArgs)
	if s.Pool != nil {
		// SSH-native: run the exec on the host. `exec` so podman inherits the remote
		// wrapper's process group and a cancel kills the exec client (which signals the
		// in-container process), matching the local group-kill semantics.
		return s.Pool.Stream(ctx, s.host(), "exec "+shellJoinCmd(s.Runtime, args))
	}
	// `exec` is itself a local child process; killing its group on abort stops the
	// exec client (and the runtime signals the in-container process).
	return startProc(ctx, s.Runtime, args, "", "start sandbox turn")
}

// Ensure creates and starts the session's long-lived container if it isn't
// already running. The container just idles (`sleep infinity`); turns run via
// exec. A stale stopped container of the same name is removed first.
func (s SandboxExecutor) Ensure(ctx context.Context, sess *Session) error {
	name, dir := sess.Container, sess.Dir
	if name == "" {
		return fmt.Errorf("sandbox session %q has no container name", sess.Name)
	}
	if s.running(ctx, name) {
		return nil
	}
	_ = s.Remove(ctx, name) // clear a stopped leftover so the name is free
	if out, err := s.ctl(ctx, s.createArgsFor(name, dir, sess.ResolvedProfile)); err != nil {
		return fmt.Errorf("create sandbox %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return nil
}

// Remove force-deletes the session's container. A missing container is not an
// error (delete is idempotent).
func (s SandboxExecutor) Remove(ctx context.Context, name string) error {
	if _, err := s.ctl(ctx, []string{"rm", "-f", name}); err != nil {
		return err
	}
	return nil
}

// List returns the names of all sandbox containers this server manages (running
// or stopped), matched by the shared name prefix.
func (s SandboxExecutor) List(ctx context.Context) ([]string, error) {
	out, err := s.ctl(ctx, []string{"ps", "-a", "--filter", "name=" + s.prefix(), "--format", "{{.Names}}"})
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
	out, err := s.ctl(ctx, []string{"inspect", "-f", "{{.State.Running}}", name})
	return err == nil && strings.TrimSpace(out) == "true"
}

// createArgs builds the argv that starts the idle session container: run detached
// in the session dir (mounted same-path so the transcript's project encoding
// matches the host), plus extra mounts and run-flags, the image, and a keep-alive
// command. Split out for unit-testing without a runtime.
func (s SandboxExecutor) createArgs(name, dir string) []string {
	return s.createArgsFor(name, dir, nil)
}

func (s SandboxExecutor) createArgsFor(name, dir string, p *ExecProfile) []string {
	image, mounts, creds, env, runArgs := s.Image, s.Mounts, []string(nil), map[string]string(nil), s.RunArgs
	homeMount := s.HomeMount
	if p != nil {
		image = p.Image
		mounts = p.Mounts
		creds = p.Creds
		env = p.Env
		runArgs = p.RunArgs
		// The home mount is profile-scoped: only profiles that carry HomeMount get the
		// host home bind-mounted, so a "locked" profile (empty HomeMount) can drop it.
		homeMount = p.HomeMount
		// A profile with no image of its own falls back to the executor's configured
		// image (SPAWNER_SANDBOX_IMAGE), so profiles need only override it when they want a
		// different one.
		if image == "" {
			image = s.Image
		}
	}
	args := []string{"run", "-d", "--name", name, "-w", dir, "-v", dir + ":" + dir}
	if homeMount != "" && homeMount != dir {
		// Whole host home, read-write, at the same path — dotfiles/.claude/checkouts
		// writable inside the sandbox exactly as on the host.
		args = append(args, "-v", homeMount+":"+homeMount)
	}
	for _, m := range mounts {
		args = append(args, "-v", m)
	}
	for _, m := range creds {
		args = append(args, "-v", m)
	}
	if len(env) > 0 {
		keys := make([]string, 0, len(env))
		for k := range env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-e", k+"="+env[k])
		}
	}
	args = append(args, runArgs...)
	args = append(args, image, "sleep", "infinity")
	return args
}

// execArgs builds the argv for one turn: exec the backend binary in the
// session's workdir inside the already-running container.
func (s SandboxExecutor) execArgs(name, dir, bin string, turnArgs []string) []string {
	args := []string{"exec", "-i", "-w", dir, name, bin}
	return append(args, turnArgs...)
}

// ExecShort runs a short command inside the session's container to completion in
// the given workdir, returning its combined output — the sandbox analogue of a
// host `sh -c`, used by Driver.RunOnTarget for the background-job registry
// (`spawner-job list` etc.), not for streaming a turn. It mirrors ctl (local exec
// vs. SSH-native over the pool) but targets the container: `<runtime> exec -w
// <dir> <container> sh -c <cmd>`.
func (s SandboxExecutor) ExecShort(ctx context.Context, container, dir, cmd string) (string, error) {
	args := []string{"exec", "-w", dir, container, "sh", "-c", cmd}
	if s.Pool != nil {
		out, err := s.Pool.Run(ctx, s.host(), shellJoinCmd(s.Runtime, args)+" 2>&1")
		return string(out), err
	}
	return runCLI(ctx, s.Runtime, args)
}

// runCLI runs a short runtime command (create/inspect/rm) to completion and
// returns its combined output. Used for lifecycle control, not for streaming a
// turn (that goes through startProc).
func runCLI(ctx context.Context, name string, args []string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// startProc launches name+args as a local child process in its own process group
// and returns a Proc over its stdout. On ctx cancel the whole group is SIGKILLed
// so a turn's tool children (a build, a sleep, or a container client) die with it,
// not just the top-level process. dir sets the working directory when non-empty.
// startErrPrefix labels a launch failure.
func startProc(ctx context.Context, name string, args []string, dir, startErrPrefix string) (Proc, error) {
	return startProcEnv(ctx, name, args, dir, startErrPrefix, nil)
}

func startProcEnv(ctx context.Context, name string, args []string, dir, startErrPrefix string, env []string) (Proc, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(cmd.Environ(), env...)
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

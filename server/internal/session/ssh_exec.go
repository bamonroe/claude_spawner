package session

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// SSHExecutor runs a session's turns on a remote host over SSH, streaming the same
// stream-json stdout the local executors produce, so Driver.Turn parses it without
// knowing the turn ran remotely. The target host is Session.Host — an explicit
// name (LocalHost for loopback), never an implicit default; the exact claude argv
// is the one Driver.Turn already builds.
type SSHExecutor struct {
	// Pool supplies the pooled, keepalive'd connection per host.
	Pool *SSHPool
	// Bin overrides the remote claude binary for ALL hosts; empty defers to the
	// per-host registry binary (Pool.binFor), then the config default, then "claude".
	Bin string
}

// Start opens a channel on the session's host connection and runs claude there. If
// the cached connection has died since the last turn, it drops it and re-dials once
// before failing, so a link that dropped between turns heals transparently.
func (e SSHExecutor) Start(ctx context.Context, s *Session, bin string, args []string) (Proc, error) {
	host := s.Host
	if host == "" {
		// SSH-native execution never defaults to the local box: a host-target
		// session must name an explicit host (LocalHost for loopback). This is what
		// lets a deployment drive only remote hosts — a hostless session is a bug,
		// not an implicit "run it here".
		return nil, fmt.Errorf("session %q has no host set", s.Name)
	}
	// bin (from the session's agent) wins; empty defers to the executor override
	// then the per-host registry entry, so a Claude session keeps today's behavior.
	if bin == "" {
		bin = e.Bin
	}
	if bin == "" {
		bin = e.Pool.binFor(host)
	}
	client, err := e.Pool.client(host)
	if err != nil {
		return nil, err
	}
	proc, err := e.run(ctx, client, s, bin, args)
	if err != nil {
		// The pooled connection may have died since the last turn; evict and re-dial
		// once. A fresh client that still fails is a real error.
		e.Pool.drop(host, client)
		client, derr := e.Pool.client(host)
		if derr != nil {
			return nil, err
		}
		proc, err = e.run(ctx, client, s, bin, args)
	}
	return proc, err
}

// run opens one SSH session (channel) on client, launches the remote claude in the
// session's directory, and wires ctx-cancel to stop it. The returned Proc streams
// the remote stdout and Waits on the remote exit.
func (e SSHExecutor) run(ctx context.Context, client *ssh.Client, s *Session, bin string, args []string) (Proc, error) {
	return streamRemote(ctx, client, remoteCommand(s.Dir, bin, args, s.ResolvedProfile.envList()))
}

// streamRemote launches `inner` (a POSIX-sh command) on client, wrapped so an abort
// can kill the whole remote process tree, and returns a Proc streaming its stdout —
// the reusable core shared by SSHExecutor.run (remote claude) and the SSH-native
// SandboxExecutor (remote `podman exec claude`). inner must exec its final process
// so it inherits the wrapper's process group (see cancelableCommand).
func streamRemote(ctx context.Context, client *ssh.Client, inner string) (Proc, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	// Read stderr out of band from the stream-json stdout: the remote command echoes
	// its process-group id there (see cancelableCommand) so a cancel can kill the
	// whole group. We also drain the rest so a chatty claude stderr can't block it.
	stderr, err := sess.StderrPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	if err := sess.Start(cancelableCommand(inner)); err != nil {
		_ = sess.Close()
		return nil, fmt.Errorf("start remote command: %w", err)
	}
	var pmu sync.Mutex
	pgid := 0
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, sshPGIDSentinel); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(rest)); err == nil {
					pmu.Lock()
					pgid = n
					pmu.Unlock()
				}
			}
		}
	}()
	// On abort, kill the remote process GROUP over a second channel on the same
	// connection (handshake-free), so claude AND any tool child it spawned die — the
	// remote analogue of the host executor's process-group SIGKILL. The signal+close
	// is a belt-and-suspenders fallback for the rare cancel that races the pgid
	// readout (a real turn runs for seconds, so the pgid is long since captured).
	stop := context.AfterFunc(ctx, func() {
		pmu.Lock()
		g := pgid
		pmu.Unlock()
		if g > 0 {
			killRemoteGroup(client, g)
		}
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
	})
	return &sshProc{sess: sess, stdout: stdout, stop: stop}, nil
}

// sshPGIDSentinel prefixes the line cancelableCommand writes to stderr carrying the
// remote process-group id, so run() can parse it back out of the stderr stream.
const sshPGIDSentinel = "__spawner_pgid__ "

// cancelableCommand wraps a turn's inner "cd … && exec claude …" so an abort can
// kill the whole remote process tree. setsid puts the command in a fresh session /
// process group (whose pgid is the wrapper shell's pid); the shell echoes that pgid
// on stderr and then execs into inner, so claude replaces the shell keeping the same
// pgid and every tool child it spawns inherits it. A cancel then kills -pgid to take
// the group down together. No PTY is requested, so the stream-json stdout stays
// clean (the pgid rides stderr).
func cancelableCommand(inner string) string {
	return "setsid sh -c " + shellQuote("echo "+sshPGIDSentinel+"$$ 1>&2; "+inner)
}

// killRemoteGroup opens a fresh channel on the live connection and SIGKILLs the
// remote process group, matching the host executor's group-kill-on-abort semantics.
// Best-effort: a failure (already exited, connection gone) is ignored.
func killRemoteGroup(client *ssh.Client, pgid int) {
	s, err := client.NewSession()
	if err != nil {
		return
	}
	defer s.Close()
	_ = s.Run(fmt.Sprintf("kill -s KILL -%d", pgid))
}

// sshProc adapts an *ssh.Session to Proc.
type sshProc struct {
	sess   *ssh.Session
	stdout io.Reader
	stop   func() bool // cancels the ctx AfterFunc; from context.AfterFunc
}

func (p *sshProc) Stdout() io.Reader { return p.stdout }

func (p *sshProc) Wait() error {
	if p.stop != nil {
		p.stop() // release the cancel hook; no-op if it already ran
	}
	err := p.sess.Wait()
	_ = p.sess.Close()
	return err
}

// remoteCommand builds the POSIX-sh command run on the remote host for one turn:
// change into the session directory, then exec claude with the given args. Every
// component is single-quoted so a prompt containing spaces, quotes, or newlines
// reaches the remote claude verbatim.
func remoteCommand(dir, bin string, args []string, env []string) string {
	var b strings.Builder
	b.WriteString("cd ")
	b.WriteString(shellQuote(dir))
	b.WriteString(" && exec ")
	wroteEnv := false
	for _, e := range env {
		if _, _, ok := strings.Cut(e, "="); ok {
			if !wroteEnv {
				b.WriteString("env ")
				wroteEnv = true
			}
			b.WriteString(shellQuote(e))
			b.WriteByte(' ')
		}
	}
	b.WriteString(shellQuote(bin))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

// shellJoinCmd renders name+args as one POSIX-sh command line, single-quoting each
// token so runtime flags, Go-template format strings, and a prompt full of spaces or
// quotes reach the remote shell verbatim — used to run the container runtime (podman)
// on the host over SSH.
func shellJoinCmd(name string, args []string) string {
	var b strings.Builder
	b.WriteString(shellQuote(name))
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(shellQuote(a))
	}
	return b.String()
}

func shellEnvCommand(env []string, cmd string) string {
	if len(env) == 0 {
		return cmd
	}
	var b strings.Builder
	b.WriteString("env")
	for _, e := range env {
		if _, _, ok := strings.Cut(e, "="); ok {
			b.WriteByte(' ')
			b.WriteString(shellQuote(e))
		}
	}
	b.WriteString(" sh -c ")
	b.WriteString(shellQuote(cmd))
	return b.String()
}

// shellQuote wraps s in single quotes for POSIX sh, escaping embedded single
// quotes as the standard '\” sequence, so arbitrary text survives the remote shell
// unchanged.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

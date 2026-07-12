package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/bam/claude_spawner/server/internal/session/bgjob"
)

// jobRootRel is the target-side directory (relative to $HOME) holding the staged
// spawner-job script and the per-dir job registry. It matches the script's own
// default ($HOME/.spawner-jobs), so a bare `spawner-job` and the server address
// the same tree without any SPAWNER_JOB_ROOT override.
const jobRootRel = ".spawner-jobs"

// JobScriptPath is the absolute path the spawner-job wrapper is staged at inside a
// target's home. Turns reference it explicitly (the priming line tells Claude to
// call it by this path), so it must not depend on PATH.
func JobScriptPath(home string) string {
	return filepath.Join(home, jobRootRel, bgjob.StagedName)
}

// RunOnTarget runs an arbitrary SHORT command on the SAME target the session's
// turns use (host direct-fork, SSH, or sandbox container), in the session's Dir so
// the dir-keyed background-job registry lines up with Claude's own invocation. It
// reuses the existing transports — a local exec, the SSH pool, or `podman exec` —
// and returns the command's combined output. Used by the background-job reconciler
// (`spawner-job list --json`, `tail`, `reap`) and by staging; it is NOT the turn
// path (turns stream via Driver.Turn).
func (d *Driver) RunOnTarget(ctx context.Context, s *Session, cmd string) ([]byte, error) {
	switch e := d.executor(s.Target).(type) {
	case SandboxExecutor:
		if s.Container == "" {
			return nil, fmt.Errorf("sandbox session %q has no container", s.Name)
		}
		out, err := e.ExecShort(ctx, s.Container, s.Dir, cmd)
		return []byte(out), err
	case SSHExecutor:
		host := s.Host
		if host == "" {
			// The store migration resolves a hostless host-target session to loopback;
			// mirror that here so RunOnTarget never fails on an unset host.
			host = LocalHost
		}
		// Run in the session Dir so the registry key (pwd) matches the turn's.
		return e.Pool.Run(ctx, host, "cd "+shellQuote(s.Dir)+" && "+cmd)
	default: // HostExecutor (or any direct-fork executor)
		c := exec.CommandContext(ctx, "sh", "-c", cmd)
		c.Dir = s.Dir
		out, err := c.CombinedOutput()
		return out, err
	}
}

// StageJobScript writes the embedded spawner-job wrapper onto the session's target
// (host, SSH, or sandbox) at JobScriptPath and makes it executable. Idempotent and
// cheap enough to call lazily per turn/reconcile. A staging failure is returned,
// never fatal — the caller logs and continues so it can NEVER block a turn.
func (d *Driver) StageJobScript(ctx context.Context, s *Session, home string) error {
	dir := filepath.Join(home, jobRootRel)
	path := JobScriptPath(home)
	// One shell command that creates the dir, writes the script from a heredoc, and
	// chmods it — so it works identically on the host, over SSH, and in a container
	// (the transports only run commands, they don't copy files). The heredoc is
	// quoted ('EOF') so the script body is written verbatim, no expansion.
	script := "mkdir -p " + shellQuote(dir) + " && cat > " + shellQuote(path) +
		" <<'SPAWNERJOBEOF'\n" + bgjob.Script + "\nSPAWNERJOBEOF\nchmod +x " + shellQuote(path)
	out, err := d.RunOnTarget(ctx, s, script)
	if err != nil {
		return fmt.Errorf("stage spawner-job on %q: %w: %s", s.Name, err, string(out))
	}
	return nil
}

// HostHome returns $HOME on the server (the host target's home). SSH and sandbox
// targets share the host user's home in this deployment (loopback SSH; the sandbox
// bind-mounts the host home), so this is the staging/reference home for every
// target here. Empty $HOME falls back to os.UserHomeDir.
func HostHome() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return ""
}

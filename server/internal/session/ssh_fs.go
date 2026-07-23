package session

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
)

// DirEntry is one subdirectory of a host's filesystem, as surfaced by the visual
// "new session" browser. Path is absolute on that host; Repo marks a git checkout.
type DirEntry struct {
	Name string
	Path string
	Repo bool
	Dir  bool // true for a subdirectory, false for a regular file (ListAll only)
}

// ListDir returns the immediate, non-hidden subdirectories of dir on host — each
// flagged if it holds a .git — sorted case-insensitively. It is the remote
// equivalent of projects.Children: the browser must reflect the *target* machine's
// filesystem (loopback for LocalHost), never the server's own, which in a container
// is just a handful of mounts. One POSIX-sh probe over the pooled connection.
func (p *SSHPool) ListDir(ctx context.Context, host, dir string) ([]DirEntry, error) {
	// The */ glob lists only subdirectories and skips dotfiles; for each, print
	// "<repo> <name>" where <repo> is 1 when it has a .git entry. A cd failure (dir
	// gone) yields an empty listing rather than an error. Run under sh -c: the login
	// shell may be zsh, whose NOMATCH aborts the whole command with exit 1 when */
	// matches nothing (a dir with only files/dotfiles), which POSIX sh leaves literal
	// (and the [ -d ] guard then skips) — otherwise such a folder fails to browse.
	script := "cd " + shellQuote(dir) + ` 2>/dev/null || exit 0; for d in */; do [ -d "$d" ] || continue; n=${d%/}; if [ -e "$n/.git" ]; then printf '1 %s\n' "$n"; else printf '0 %s\n' "$n"; fi; done`
	out, err := p.Run(ctx, host, "sh -c "+shellQuote(script))
	if err != nil {
		return nil, err
	}
	var entries []DirEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 3 { // "<flag> <name>" — at least a flag, a space, one char
			continue
		}
		name := line[2:]
		entries = append(entries, DirEntry{Name: name, Path: joinRemote(dir, name), Repo: line[0] == '1', Dir: true})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})
	return entries, nil
}

// ListAll returns the immediate, non-hidden entries of dir on host — both
// subdirectories (Dir=true, .git-flagged) and regular files (Dir=false) — sorted
// case-insensitively with directories first. It backs the file-transfer picker,
// which must show files to download, whereas the new-session picker (ListDir) only
// walks directories. One POSIX-sh probe over the pooled connection.
func (p *SSHPool) ListAll(ctx context.Context, host, dir string) ([]DirEntry, error) {
	// For each non-hidden entry print a 2-char tag then the name: "d1"/"d0" for a
	// directory (1 = holds a .git), "f0" for a regular file. A cd failure yields an
	// empty listing rather than an error. Run under sh -c so a zsh login shell's
	// NOMATCH can't abort an empty directory's listing with exit 1 (see ListDir).
	script := "cd " + shellQuote(dir) + ` 2>/dev/null || exit 0; for e in *; do if [ -d "$e" ]; then if [ -e "$e/.git" ]; then printf 'd1 %s\n' "$e"; else printf 'd0 %s\n' "$e"; fi; elif [ -f "$e" ]; then printf 'f0 %s\n' "$e"; fi; done`
	out, err := p.Run(ctx, host, "sh -c "+shellQuote(script))
	if err != nil {
		return nil, err
	}
	var entries []DirEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if len(line) < 4 { // "dx <name>" — at least a 2-char tag, a space, one char
			continue
		}
		name := line[3:]
		entries = append(entries, DirEntry{Name: name, Path: joinRemote(dir, name), Repo: line[1] == '1', Dir: line[0] == 'd'})
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Dir != entries[b].Dir {
			return entries[a].Dir // directories first
		}
		return strings.ToLower(entries[a].Name) < strings.ToLower(entries[b].Name)
	})
	return entries, nil
}

// DirExists reports whether dir is a directory on host.
func (p *SSHPool) DirExists(ctx context.Context, host, dir string) (bool, error) {
	out, err := p.Run(ctx, host, "if [ -d "+shellQuote(dir)+" ]; then printf 1; fi")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "1", nil
}

// MakeDir creates dir and any missing parents on host.
func (p *SSHPool) MakeDir(ctx context.Context, host, dir string) error {
	_, err := p.Run(ctx, host, "mkdir -p "+shellQuote(dir))
	return err
}

// ReadFile returns the bytes of path on host, streamed over the pooled connection
// (the SSH channel is binary-clean, so `cat` reproduces the file verbatim).
func (p *SSHPool) ReadFile(ctx context.Context, host, path string) ([]byte, error) {
	return p.Run(ctx, host, "cat "+shellQuote(path))
}

// WriteFile writes data to path on host, truncating any existing file and creating
// the parent directory. The bytes travel as the remote `cat`'s stdin, so they land
// verbatim regardless of content. Re-dials once on a stale cached connection.
func (p *SSHPool) WriteFile(ctx context.Context, host, path string, data []byte) error {
	client, err := p.client(host)
	if err != nil {
		return err
	}
	err = writeRemote(ctx, client, path, data)
	if err != nil {
		p.drop(host, client)
		client, derr := p.client(host)
		if derr != nil {
			return err
		}
		err = writeRemote(ctx, client, path, data)
	}
	return err
}

// writeRemote opens one session channel and pipes data into `cat > path` (making the
// parent dir first). ctx-cancel kills the remote command so a hung write can't leak
// a channel.
func writeRemote(ctx context.Context, client *ssh.Client, path string, data []byte) error {
	sess, err := client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { _ = sess.Signal(ssh.SIGKILL); _ = sess.Close() })
	defer stop()
	sess.Stdin = bytes.NewReader(data)
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	dir := filepath.Dir(path)
	if err := sess.Run("mkdir -p " + shellQuote(dir) + " && cat > " + shellQuote(path)); err != nil {
		// sess.Run's ExitError is just "status N" — fold in the remote command's
		// stderr (e.g. "Permission denied", "Not a directory") so an upload failure
		// reports why, not an opaque status code.
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s: %w", msg, err)
		}
		return err
	}
	return nil
}

// Run executes a short command on host over the pooled connection and returns its
// stdout. For the browser's filesystem probes (list/stat/mkdir) — turns stream via
// SSHExecutor. Re-dials once if the cached connection has gone stale, mirroring
// SSHExecutor.Start.
func (p *SSHPool) Run(ctx context.Context, host, cmd string) ([]byte, error) {
	client, err := p.client(host)
	if err != nil {
		return nil, err
	}
	out, err := runRemote(ctx, client, cmd)
	if err != nil {
		p.drop(host, client)
		client, derr := p.client(host)
		if derr != nil {
			return nil, err
		}
		out, err = runRemote(ctx, client, cmd)
	}
	return out, err
}

// Stream launches inner on host over the pooled connection and returns a Proc
// streaming its stdout — the streaming analogue of Run, used by the SSH-native
// SandboxExecutor to run a turn's `podman exec claude` on the host. Re-dials once if
// the cached connection has gone stale, mirroring SSHExecutor.Start.
func (p *SSHPool) Stream(ctx context.Context, host, inner string) (Proc, error) {
	client, err := p.client(host)
	if err != nil {
		return nil, err
	}
	proc, err := streamRemote(ctx, client, inner)
	if err != nil {
		p.drop(host, client)
		client, derr := p.client(host)
		if derr != nil {
			return nil, err
		}
		proc, err = streamRemote(ctx, client, inner)
	}
	return proc, err
}

// runRemote opens one session channel, runs cmd, and returns its stdout. ctx-cancel
// kills the remote command so a hung probe can't leak a channel.
func runRemote(ctx context.Context, client *ssh.Client, cmd string) ([]byte, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { _ = sess.Signal(ssh.SIGKILL); _ = sess.Close() })
	defer stop()
	return sess.Output(cmd)
}

// joinRemote joins a POSIX absolute dir and a child name without doubling the slash
// at the filesystem root.
func joinRemote(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return strings.TrimRight(dir, "/") + "/" + name
}

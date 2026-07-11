package session

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// claudeFS accesses the Claude on-disk state — the session transcripts under
// ~/.claude/projects — on ONE machine: the local host (via os.*) when remote is
// nil, or a remote host over SSH when set. It is the seam that lets discovery,
// history, and last-context-usage work for an SSH-native session whose transcripts
// live on another box, without the higher-level parse logic knowing which host it's
// reading. Only the raw file access (glob/stat/open/remove) differs by backend; the
// JSONL parsing above these primitives is shared.
type claudeFS struct {
	remote *sshFS // nil = local machine
}

// sshFS reaches a remote host's Claude state over the pooled SSH connection.
type sshFS struct {
	pool *SSHPool
	host string
}

// localClaudeFS is the local-machine backend used by the package-level wrappers
// (DiscoverSessions, ReadTranscript, …), preserving today's behavior.
var localClaudeFS = claudeFS{}

// errRemoteMissing marks a remote file that couldn't be read (absent/unreadable),
// so the shared parse code can treat it like a local os.IsNotExist and return empty.
var errRemoteMissing = errors.New("remote claude file missing")

// transcriptRef is one transcript path plus its last-modified time, as returned by
// listAllTranscripts (mtime included so remote discovery needs no per-file stat).
type transcriptRef struct {
	path string
	mod  time.Time
}

// cacheKey namespaces the transcript parse cache by host, so an identical absolute
// path on two different machines (e.g. /home/bam/.claude/... locally and on a
// remote) can't collide in the shared map.
func (fs claudeFS) cacheKey(path string) string {
	if fs.remote == nil {
		return path
	}
	return fs.remote.host + "\x00" + path
}

// output runs a short command on the remote host and returns its stdout. Used for
// the file-access primitives (find/ls/stat/cat/rm), not for streaming a turn.
func (r *sshFS) output(cmd string) ([]byte, error) {
	c, err := r.pool.client(r.host)
	if err != nil {
		return nil, err
	}
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Output(cmd)
}

// listAllTranscripts returns every session transcript under ~/.claude/projects with
// its mtime, newest-first ordering left to the caller.
func (fs claudeFS) listAllTranscripts() ([]transcriptRef, error) {
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
		refs := make([]transcriptRef, 0, len(matches))
		for _, p := range matches {
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			refs = append(refs, transcriptRef{path: p, mod: fi.ModTime()})
		}
		return refs, nil
	}
	// One round trip: mtime + path for every transcript. `|| true` so a missing
	// projects dir yields an empty list rather than a non-zero exit / error.
	out, err := fs.remote.output(`find "$HOME/.claude/projects" -maxdepth 2 -name '*.jsonl' -printf '%T@ %p\n' 2>/dev/null || true`)
	if err != nil {
		return nil, err
	}
	var refs []transcriptRef
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, " ", 2)
		if len(parts) != 2 {
			continue
		}
		secs, err := strconv.ParseFloat(parts[0], 64)
		if err != nil {
			continue
		}
		refs = append(refs, transcriptRef{path: parts[1], mod: time.Unix(int64(secs), 0)})
	}
	return refs, nil
}

// findByID returns the transcript path for a session_id (globbed across the opaque
// project-dir encoding), or "" if not found.
func (fs claudeFS) findByID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sessionID+".jsonl"))
		if len(matches) > 0 {
			return matches[0]
		}
		return ""
	}
	if !looksLikeUUID(sessionID) {
		return "" // guard the value we interpolate into the remote glob
	}
	out, err := fs.remote.output(`ls -1d "$HOME/.claude/projects/"*/` + sessionID + `.jsonl 2>/dev/null || true`)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// listDirTranscripts returns the *.jsonl transcripts in the same project folder as
// dir (an absolute transcript-sibling directory on the target host).
func (fs claudeFS) listDirTranscripts(dir string) []string {
	if fs.remote == nil {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		return matches
	}
	out, err := fs.remote.output(`ls -1d ` + shellQuote(dir) + `/*.jsonl 2>/dev/null || true`)
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			files = append(files, line)
		}
	}
	return files
}

// stat returns a transcript's size and mtime (the parse-cache key). ok is false
// when the file can't be stat'd, so the caller parses uncached rather than trusting
// a stale cache entry.
func (fs claudeFS) stat(path string) (size int64, mod time.Time, ok bool) {
	if fs.remote == nil {
		fi, err := os.Stat(path)
		if err != nil {
			return 0, time.Time{}, false
		}
		return fi.Size(), fi.ModTime(), true
	}
	out, err := fs.remote.output(`stat -c '%s %Y' ` + shellQuote(path) + ` 2>/dev/null`)
	if err != nil {
		return 0, time.Time{}, false
	}
	parts := strings.Fields(strings.TrimSpace(string(out)))
	if len(parts) != 2 {
		return 0, time.Time{}, false
	}
	sz, err1 := strconv.ParseInt(parts[0], 10, 64)
	secs, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, time.Time{}, false
	}
	return sz, time.Unix(secs, 0), true
}

// open returns a reader over a transcript's contents. For the remote backend the
// whole file is fetched (cat) into memory — the working set is a handful of
// sessions; a genuinely missing/unreadable file yields errRemoteMissing so the
// caller treats it like a local os.IsNotExist.
func (fs claudeFS) open(path string) (io.ReadCloser, error) {
	if fs.remote == nil {
		return os.Open(path)
	}
	out, err := fs.remote.output(`cat ` + shellQuote(path))
	if err != nil {
		return nil, errRemoteMissing
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

// remove deletes a transcript file (idempotent; a missing file is not an error).
func (fs claudeFS) remove(path string) error {
	if fs.remote == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_, err := fs.remote.output(`rm -f ` + shellQuote(path))
	return err
}

// isMissing reports whether an open error means the file is absent, for either
// backend, so the shared parse code returns empty rather than surfacing it.
func (fs claudeFS) isMissing(err error) bool {
	if fs.remote == nil {
		return os.IsNotExist(err)
	}
	return errors.Is(err, errRemoteMissing)
}

// discoverSessions scans the target host's ~/.claude/projects for every session
// transcript and returns each session's id + working directory + last-active time,
// newest first, one row per directory. (Backend-neutral; see DiscoverSessions.)
func (fs claudeFS) discoverSessions() ([]Discovered, error) {
	refs, err := fs.listAllTranscripts()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]Discovered, 0, len(refs))
	for _, ref := range refs {
		id := strings.TrimSuffix(filepath.Base(ref.path), ".jsonl")
		if !looksLikeUUID(id) || seen[id] {
			continue
		}
		dir := fs.transcriptCwd(ref.path)
		if dir == "" {
			continue
		}
		seen[id] = true
		out = append(out, Discovered{SessionID: id, Dir: dir, LastActive: ref.mod.Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive > out[j].LastActive })
	// One entry per directory: keep the most-recently-active session (the one
	// `claude --resume` would continue), not every historical session in that dir.
	byDir := map[string]bool{}
	deduped := out[:0]
	for _, d := range out {
		if byDir[d.Dir] {
			continue
		}
		byDir[d.Dir] = true
		deduped = append(deduped, d)
	}
	return deduped, nil
}

// perSessionStateDirs are the ~/.claude subdirectories Claude fills with a
// session's ancillary state keyed by its session_id — everything beyond the
// transcript .jsonl: subagent logs and cached tool results (a sibling
// projects/<dir>/<id>/ dir), the session's task list, its file-edit history, and
// its per-session shell env. A full delete wipes all of them so nothing about
// the session lingers on disk.
var perSessionStateDirs = []string{"tasks", "file-history", "session-env"}

// removeAll recursively deletes a path (file or directory), tolerating absence —
// the recursive counterpart to remove, for the per-session state directories.
func (fs claudeFS) removeAll(path string) error {
	if fs.remote == nil {
		return os.RemoveAll(path)
	}
	_, err := fs.remote.output(`rm -rf ` + shellQuote(path))
	return err
}

// purgeSessionState removes the per-session state directories keyed by a bare
// session_id (perSessionStateDirs), under ~/.claude. The transcript and its
// projects/<dir>/<id>/ sidecar are handled by the caller (they hang off the
// transcript path); this covers the state that lives outside projects/. id is
// UUID-validated before it's interpolated into any path/shell command.
func (fs claudeFS) purgeSessionState(id string) error {
	if !looksLikeUUID(id) {
		return nil // never build a path/command from an untrusted value
	}
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		for _, sub := range perSessionStateDirs {
			if err := os.RemoveAll(filepath.Join(home, ".claude", sub, id)); err != nil {
				return err
			}
		}
		return nil
	}
	var b strings.Builder
	for _, sub := range perSessionStateDirs {
		// id is UUID-validated and sub is a constant, so $HOME can expand unquoted.
		b.WriteString(` "$HOME/.claude/` + sub + `/` + id + `"`)
	}
	_, err := fs.remote.output("rm -rf" + b.String())
	return err
}

// purgeByID removes every on-disk trace of one session_id: its transcript, the
// projects/<dir>/<id>/ sidecar alongside it (subagents + tool results), and its
// per-session state dirs. Missing paths are fine. Reports whether a transcript
// existed, so callers can count real deletions.
func (fs claudeFS) purgeByID(id string) (bool, error) {
	had := false
	if p := fs.findByID(id); p != "" {
		// Transcript and sidecar share a stem: projects/<dir>/<id>.jsonl and
		// projects/<dir>/<id>/. Deriving the sidecar from the found path hits the
		// right (opaque) project-dir encoding without re-globbing.
		sidecar := strings.TrimSuffix(p, ".jsonl")
		if err := fs.remove(p); err != nil {
			return false, err
		}
		if err := fs.removeAll(sidecar); err != nil {
			return true, err
		}
		had = true
	}
	if err := fs.purgeSessionState(id); err != nil {
		return had, err
	}
	return had, nil
}

// deleteByIDs fully purges each session_id (transcript + sidecar + per-session
// state), leaving dir-mates intact. Returns how many transcripts existed.
func (fs claudeFS) deleteByIDs(ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		had, err := fs.purgeByID(id)
		if err != nil {
			return n, err
		}
		if had {
			n++
		}
	}
	return n, nil
}

// deleteForDir fully purges EVERY session whose working directory is dir (legacy
// whole-directory delete). anySessionID locates the project folder.
func (fs claudeFS) deleteForDir(anySessionID, dir string) (int, error) {
	path := fs.findByID(anySessionID)
	if path == "" {
		return 0, nil
	}
	n := 0
	for _, f := range fs.listDirTranscripts(filepath.Dir(path)) {
		if fs.transcriptCwd(f) != dir {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if _, err := fs.purgeByID(id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// remoteClaudeFor returns the claudeFS for a session: the local backend when the
// session runs on this machine (Host empty), or an SSH backend on Session.Host when
// SSH-native execution is configured. Falls back to local if SSH isn't wired.
func (d *Driver) claudeFSFor(host string) claudeFS {
	if host == "" {
		return localClaudeFS
	}
	if ex, ok := d.Execs[TargetHost].(SSHExecutor); ok && ex.Pool != nil {
		return claudeFS{remote: &sshFS{pool: ex.Pool, host: host}}
	}
	return localClaudeFS
}

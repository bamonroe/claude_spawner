package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
// It goes through SSHPool.Run rather than opening a channel itself so these
// probes are inside the connection's channel budget: the digest sweep fans
// several of them out at once, and un-budgeted they were what pushed a pooled
// client past the peer's MaxSessions and got it wrongly dropped as stale.
// The caller's context is honored and additionally capped at remoteProbeTimeout:
// the deadline bounds the wait for a channel slot as much as the command, so an
// unbounded wait behind a busy connection fails a read rather than hanging it. A
// caller with a tighter deadline (the digest sweep's per-session budget) wins.
func (r *sshFS) output(ctx context.Context, cmd string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, remoteProbeTimeout)
	defer cancel()
	return r.pool.Run(ctx, r.host, cmd)
}

// remoteProbeTimeout caps one file-access probe (find/ls/stat/cat/rm) end to end,
// including any wait for a channel slot.
const remoteProbeTimeout = 30 * time.Second

// listAllTranscripts returns every session transcript under ~/.claude/projects with
// its mtime, newest-first ordering left to the caller.
func (fs claudeFS) listAllTranscripts(ctx context.Context) ([]transcriptRef, error) {
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
	out, err := fs.remote.output(ctx, `find "$HOME/.claude/projects" -maxdepth 2 -name '*.jsonl' -printf '%T@ %p\n' 2>/dev/null || true`)
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

// The id→path resolution is memoized per host. A session_id's transcript lives at
// exactly one path for the life of that transcript — the project-dir encoding is
// derived from the working directory and neither moves — so this mapping is stable,
// while recomputing it is an SSH round trip (`ls`) paid before every stat, every
// read, and once per id in every chain signature.
//
// The entry is dropped the moment a resolved path stops working (a failed stat, a
// missing file, a delete), so an out-of-band removal or a project-dir rename
// self-heals on the next call rather than pinning a path that no longer exists.
var (
	transcriptPathMu    sync.Mutex
	transcriptPathCache = map[string]string{}
)

// forgetPath drops a memoized id→path entry so the next findByID re-resolves.
func (fs claudeFS) forgetPath(sessionID string) {
	transcriptPathMu.Lock()
	delete(transcriptPathCache, fs.cacheKey(sessionID))
	transcriptPathMu.Unlock()
}

// findByID returns the transcript path for a session_id (globbed across the opaque
// project-dir encoding), or "" if not found. A successful resolution is memoized;
// see transcriptPathCache.
func (fs claudeFS) findByID(ctx context.Context, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if fs.remote == nil {
		// Local resolution is a filepath.Glob — cheap, and relative to $HOME, which
		// is not part of the cache key. Only the remote backend (an SSH round trip
		// per lookup) is worth memoizing, and it is the one that hurt.
		return fs.resolveByID(ctx, sessionID)
	}
	key := fs.cacheKey(sessionID)
	transcriptPathMu.Lock()
	cached, hit := transcriptPathCache[key]
	transcriptPathMu.Unlock()
	if hit {
		return cached
	}
	path := fs.resolveByID(ctx, sessionID)
	if path != "" {
		transcriptPathMu.Lock()
		transcriptPathCache[key] = path
		transcriptPathMu.Unlock()
	}
	return path
}

// resolveByID is findByID's uncached lookup: the actual glob on the target host.
func (fs claudeFS) resolveByID(ctx context.Context, sessionID string) string {
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
	out, err := fs.remote.output(ctx, `ls -1d "$HOME/.claude/projects/"*/`+sessionID+`.jsonl 2>/dev/null || true`)
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
func (fs claudeFS) listDirTranscripts(ctx context.Context, dir string) []string {
	if fs.remote == nil {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
		return matches
	}
	out, err := fs.remote.output(ctx, `ls -1d `+shellQuote(dir)+`/*.jsonl 2>/dev/null || true`)
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
func (fs claudeFS) stat(ctx context.Context, path string) (size int64, mod time.Time, ok bool) {
	st, hit := fs.statMany(ctx, []string{path})[path]
	return st.size, st.mod, hit
}

// fileStat is one path's size and mtime, as returned by statMany.
type fileStat struct {
	size int64
	mod  time.Time
}

// statMany stats a batch of paths in ONE remote exec (chunked), returning only the
// paths that stat'd — a missing entry means "couldn't stat", the same signal
// stat's ok=false carries. This is the seam every freshness check goes through:
// signing a chain of N transcripts is one round trip, not N.
func (fs claudeFS) statMany(ctx context.Context, paths []string) map[string]fileStat {
	out := make(map[string]fileStat, len(paths))
	if len(paths) == 0 {
		return out
	}
	if fs.remote == nil {
		for _, p := range paths {
			if fi, err := os.Stat(p); err == nil {
				out[p] = fileStat{size: fi.Size(), mod: fi.ModTime()}
			}
		}
		return out
	}
	for start := 0; start < len(paths); start += cwdsChunk {
		end := min(start+cwdsChunk, len(paths))
		var cmd strings.Builder
		cmd.WriteString("for p in")
		for _, p := range paths[start:end] {
			cmd.WriteString(" " + shellQuote(p))
		}
		// One "path<TAB>size mtime" line per file; a file that doesn't stat prints
		// just the path, so it's simply absent from the map.
		// The path is printed separately from stat's format so a '%' in a path
		// can never be read as a format directive.
		cmd.WriteString(`; do printf '%s\t' "$p"; stat -c '%s %Y' "$p" 2>/dev/null; echo; done`)
		blob, err := fs.remote.output(ctx, cmd.String())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(blob), "\n") {
			path, rest, ok := strings.Cut(strings.TrimRight(line, "\r"), "\t")
			if !ok {
				continue
			}
			fields := strings.Fields(rest)
			if len(fields) != 2 {
				continue // no stat output: the file isn't there
			}
			sz, err1 := strconv.ParseInt(fields[0], 10, 64)
			secs, err2 := strconv.ParseInt(fields[1], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			out[path] = fileStat{size: sz, mod: time.Unix(secs, 0)}
		}
	}
	return out
}

// open returns a reader over a transcript's contents. For the remote backend the
// whole file is fetched (cat) into memory — the working set is a handful of
// sessions; a genuinely missing/unreadable file yields errRemoteMissing so the
// caller treats it like a local os.IsNotExist.
func (fs claudeFS) open(ctx context.Context, path string) (io.ReadCloser, error) {
	if fs.remote == nil {
		return os.Open(path)
	}
	out, err := fs.remote.output(ctx, `cat `+shellQuote(path))
	if err != nil {
		return nil, errRemoteMissing
	}
	return io.NopCloser(bytes.NewReader(out)), nil
}

// tailBytes returns the last n bytes of a transcript, and whether that span
// covers the WHOLE file (so the caller knows the first line isn't truncated and
// that there is nothing earlier left to look at).
//
// It is the tail-side counterpart of cwdHeadBytes' bounded head read, and exists
// for the same reason: the expensive resource here is bytes on the wire. The
// last-context-usage lookup only ever needs the end of the file, but transcripts
// reach tens of megabytes, and reading one through `open` is a full `cat` over
// SSH — paid once per turn, right in the critical path, on a file the turn just
// grew (so the parse cache is guaranteed cold).
func (fs claudeFS) tailBytes(ctx context.Context, path string, n int64) (data []byte, whole bool, err error) {
	if n <= 0 {
		return nil, false, nil
	}
	if fs.remote == nil {
		f, err := os.Open(path)
		if err != nil {
			return nil, false, err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return nil, false, err
		}
		off, size := int64(0), fi.Size()
		if size > n {
			off = size - n
		}
		buf := make([]byte, size-off)
		if _, err := io.ReadFull(io.NewSectionReader(f, off, size-off), buf); err != nil && err != io.EOF {
			return nil, false, err
		}
		return buf, off == 0, nil
	}
	out, err := fs.remote.output(ctx, fmt.Sprintf("tail -c %d %s", n, shellQuote(path)))
	if err != nil {
		return nil, false, errRemoteMissing
	}
	// Short of the budget means we got everything there was.
	return out, int64(len(out)) < n, nil
}

// cwdHeadBytes bounds how far into a transcript the working-directory lookup
// reads. `cwd` is recorded on essentially every event, so it's in the first line
// or two — but a single line can be megabytes (a big tool result), which is why
// this is a BYTE budget and not a line count. Measured against a real 286-file
// ~560 MB projects dir: 64 KiB recovers every cwd that exists at all (the handful
// it misses have no cwd anywhere in the file, at any budget).
const cwdHeadBytes = 64 << 10

// cwdsChunk caps how many paths go into one remote command, keeping the argument
// list well inside ARG_MAX.
const cwdsChunk = 200

// cwdPattern matches a JSON `"cwd":"…"` member whose value needs no unescaping —
// the remote extractor's grep. Values containing a backslash escape are skipped
// rather than mis-parsed; on Linux a working directory with a quote or backslash
// in it doesn't occur in practice, and the cost of missing one is a session
// showing up unadopted rather than anything corrupt.
const cwdPattern = `"cwd":"[^"\\]*"`

// cwds recovers each transcript's working directory. It is the seam that keeps
// metadata reads cheap on BOTH backends, which the callers can't do for
// themselves: locally it reads a bounded head and parses the JSON exactly;
// remotely it extracts the value on the far side and ships back one short line
// per file, because the expensive resource over SSH is bytes on the wire, not
// commands run.
//
// It exists because the obvious implementations are catastrophic at real sizes.
// Reading each transcript through `open` (a full `cat` remotely) moved ~560 MB
// and never finished — the app connected and then sat with an empty session list.
// Shipping back bounded heads to parse locally still moved ~40 MB at the ~1 MB/s
// this loopback SSH channel sustains, i.e. ~44 s per sweep. Extracting remotely
// makes the payload a few KB. Paths whose cwd can't be recovered are absent from
// the result.
func (fs claudeFS) cwds(ctx context.Context, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	if fs.remote == nil {
		for _, p := range paths {
			if dir := localCwd(p); dir != "" {
				out[p] = dir
			}
		}
		return out
	}
	for start := 0; start < len(paths); start += cwdsChunk {
		end := min(start+cwdsChunk, len(paths))
		var cmd strings.Builder
		cmd.WriteString("for p in")
		for _, p := range paths[start:end] {
			cmd.WriteString(" " + shellQuote(p))
		}
		// One tab-separated "path<TAB>match" line per file. head bounds the read,
		// grep -m1 stops at the first hit, and `|| true` keeps a miss (grep exits 1)
		// from failing the whole command.
		fmt.Fprintf(&cmd, `; do printf '%%s\t' "$p"; { head -c %d "$p" 2>/dev/null | grep -aom1 %s || true; }; echo; done`,
			cwdHeadBytes, shellQuote(cwdPattern))
		blob, err := fs.remote.output(ctx, cmd.String())
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(blob), "\n") {
			path, match, ok := strings.Cut(line, "\t")
			if !ok {
				continue
			}
			if dir := cwdFromMatch(match); dir != "" {
				out[path] = dir
			}
		}
	}
	return out
}

// localCwd reads a bounded head of a local transcript and returns the first `cwd`
// it records, parsing the JSON properly (no pattern-matching needed locally).
func localCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, cwdHeadBytes))
	if err != nil {
		return ""
	}
	for _, line := range bytes.Split(head, []byte("\n")) {
		var ev struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(line, &ev) == nil && ev.Cwd != "" {
			return ev.Cwd
		}
	}
	return ""
}

// cwdFromMatch unwraps the remote extractor's `"cwd":"…"` match into the bare
// path. The pattern excludes escapes, so the value is literal.
func cwdFromMatch(match string) string {
	const prefix = `"cwd":"`
	match = strings.TrimSpace(match)
	if !strings.HasPrefix(match, prefix) || !strings.HasSuffix(match, `"`) {
		return ""
	}
	return match[len(prefix) : len(match)-1]
}

// remove deletes a transcript file (idempotent; a missing file is not an error).
func (fs claudeFS) remove(ctx context.Context, path string) error {
	if fs.remote == nil {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	_, err := fs.remote.output(ctx, `rm -f `+shellQuote(path))
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
// newest first — EVERY session, including dir-mates. Collapsing to one row per
// directory is the caller's choice (the gateway does it only for unregistered,
// adoptable rows): collapsing here starved registered dir-mates of their own
// last-active time, sinking them to the bottom of the app's sidebar.
// (Backend-neutral; see DiscoverSessions.)
func (fs claudeFS) discoverSessions(ctx context.Context) ([]Discovered, error) {
	refs, err := fs.listAllTranscripts(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	// Recover every transcript's working directory in one batch (a couple of remote
	// commands) rather than a per-file read — see heads.
	paths := make([]string, 0, len(refs))
	for _, ref := range refs {
		if id := strings.TrimSuffix(filepath.Base(ref.path), ".jsonl"); looksLikeUUID(id) {
			paths = append(paths, ref.path)
		}
	}
	dirByPath := fs.cwds(ctx, paths)
	out := make([]Discovered, 0, len(refs))
	for _, ref := range refs {
		id := strings.TrimSuffix(filepath.Base(ref.path), ".jsonl")
		if !looksLikeUUID(id) || seen[id] {
			continue
		}
		dir := dirByPath[ref.path]
		if dir == "" {
			continue
		}
		seen[id] = true
		out = append(out, Discovered{SessionID: id, Dir: dir, LastActive: ref.mod.Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive > out[j].LastActive })
	return out, nil
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
func (fs claudeFS) removeAll(ctx context.Context, path string) error {
	if fs.remote == nil {
		return os.RemoveAll(path)
	}
	_, err := fs.remote.output(ctx, `rm -rf `+shellQuote(path))
	return err
}

// purgeSessionState removes the per-session state directories keyed by a bare
// session_id (perSessionStateDirs), under ~/.claude. The transcript and its
// projects/<dir>/<id>/ sidecar are handled by the caller (they hang off the
// transcript path); this covers the state that lives outside projects/. id is
// UUID-validated before it's interpolated into any path/shell command.
func (fs claudeFS) purgeSessionState(ctx context.Context, id string) error {
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
	_, err := fs.remote.output(ctx, "rm -rf"+b.String())
	return err
}

// purgeByID removes every on-disk trace of one session_id: its transcript, the
// projects/<dir>/<id>/ sidecar alongside it (subagents + tool results), and its
// per-session state dirs. Missing paths are fine. Reports whether a transcript
// existed, so callers can count real deletions.
func (fs claudeFS) purgeByID(ctx context.Context, id string) (bool, error) {
	had := false
	defer fs.forgetPath(id) // the path is about to stop existing
	if p := fs.findByID(ctx, id); p != "" {
		// Transcript and sidecar share a stem: projects/<dir>/<id>.jsonl and
		// projects/<dir>/<id>/. Deriving the sidecar from the found path hits the
		// right (opaque) project-dir encoding without re-globbing.
		sidecar := strings.TrimSuffix(p, ".jsonl")
		if err := fs.remove(ctx, p); err != nil {
			return false, err
		}
		if err := fs.removeAll(ctx, sidecar); err != nil {
			return true, err
		}
		had = true
	}
	if err := fs.purgeSessionState(ctx, id); err != nil {
		return had, err
	}
	return had, nil
}

// deleteByIDs fully purges each session_id (transcript + sidecar + per-session
// state), leaving dir-mates intact. Returns how many transcripts existed.
//
// On a remote host this is ONE exec for the whole batch: the serial form paid
// three or four SSH round trips per id (glob, rm, rm -rf sidecar, rm -rf state),
// which is what made deleting a much-cleared session — many rotated prior ids —
// take ten to thirty round trips before the row disappeared.
func (fs claudeFS) deleteByIDs(ctx context.Context, ids []string) (int, error) {
	if fs.remote != nil {
		return fs.deleteByIDsRemote(ctx, ids)
	}
	n := 0
	for _, id := range ids {
		had, err := fs.purgeByID(ctx, id)
		if err != nil {
			return n, err
		}
		if had {
			n++
		}
	}
	return n, nil
}

// deleteByIDsRemote is deleteByIDs' batched remote form: a single shell command
// that, for every id, reports whether a transcript existed and then removes the
// transcript, its sibling sidecar dir, and every per-session state dir. The globs
// stand in for the opaque project-dir encoding, so no separate lookup is needed;
// an unmatched glob stays literal and `rm -rf` on it is a no-op. Every id is
// UUID-validated before it reaches the command.
func (fs claudeFS) deleteByIDsRemote(ctx context.Context, ids []string) (int, error) {
	var safe []string
	for _, id := range ids {
		if looksLikeUUID(id) {
			safe = append(safe, id)
		}
	}
	if len(safe) == 0 {
		return 0, nil
	}
	defer func() {
		for _, id := range safe {
			fs.forgetPath(id) // the paths are about to stop existing
		}
	}()
	var state strings.Builder
	for _, sub := range perSessionStateDirs {
		state.WriteString(` "$HOME/.claude/` + sub + `/$id"`)
	}
	cmd := `for id in ` + strings.Join(safe, " ") + `; do ` +
		`for p in "$HOME/.claude/projects/"*/"$id.jsonl"; do [ -e "$p" ] && echo "$id"; done; ` +
		`rm -rf "$HOME/.claude/projects/"*/"$id.jsonl" "$HOME/.claude/projects/"*/"$id"` + state.String() + `; ` +
		`done`
	out, err := fs.remote.output(ctx, cmd)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n, nil
}

// deleteForDir fully purges EVERY session whose working directory is dir (legacy
// whole-directory delete). anySessionID locates the project folder.
func (fs claudeFS) deleteForDir(ctx context.Context, anySessionID, dir string) (int, error) {
	path := fs.findByID(ctx, anySessionID)
	if path == "" {
		return 0, nil
	}
	n := 0
	for _, f := range fs.listDirTranscripts(ctx, filepath.Dir(path)) {
		if fs.transcriptCwd(ctx, f) != dir {
			continue
		}
		id := strings.TrimSuffix(filepath.Base(f), ".jsonl")
		if _, err := fs.purgeByID(ctx, id); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// claudeFSFor returns the claudeFS backend for a session's on-disk Claude state on
// the given host. With the SSH pool wired (SSH-native), EVERY host reads over SSH —
// an explicit Session.Host, and an empty host (sandbox turns and local discovery)
// maps to the loopback host — so the server never touches its own filesystem for
// Claude state. Falls back to the in-process local backend only when SSH isn't wired.
func (d *Driver) claudeFSFor(host string) claudeFS {
	if pool := d.hostPool(); pool != nil {
		if host == "" {
			host = LocalHost
		}
		return claudeFS{remote: &sshFS{pool: pool, host: host}}
	}
	return localClaudeFS
}

// statChainSig builds a chain's freshness signature from each transcript's path,
// size and mtime — the same proxy the in-memory parse cache trusts. resolve maps
// a chain id to its transcript path for the calling backend.
//
// A transcript that isn't there — the id resolves to nothing, or the path doesn't
// stat — is recorded as absent rather than abandoning the signature. "No
// transcript yet" is a legitimate, stable state: a freshly rotated session id, or
// a backend like antigravity whose resolver computes a deterministic path that
// may not exist. It contributes no messages, and the signature changes the moment
// the file appears, so it invalidates correctly.
//
// Treating a failed stat as "give up" instead made such a session permanently
// uncacheable — it re-parsed its entire chain on every sweep, forever. A stat
// that fails transiently is the same shape as one that fails because the file is
// missing, and that's safe here: the entry it pins is replaced as soon as the
// stat works again.
// The whole chain is stat'd in ONE batch (statMany), so a freshness check is a
// single round trip however long the chain is; only ids whose memoized path failed
// to stat cost a second, smaller batch after re-resolving.
func statChainSig(ctx context.Context, fs claudeFS, resolve func(context.Context, string) string, ids []string) (string, bool) {
	paths := make([]string, len(ids))
	var want []string
	for i, id := range ids {
		paths[i] = resolve(ctx, id)
		if paths[i] != "" {
			want = append(want, paths[i])
		}
	}
	stats := fs.statMany(ctx, want)

	// The memoized path no longer stats: re-resolve once before believing the file
	// is gone, so a project-dir rename or an out-of-band move recovers immediately
	// instead of reporting a permanently absent chain. Retries batch together too.
	var retry []string
	retryFor := map[int]string{}
	for i, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := stats[path]; ok {
			continue
		}
		fs.forgetPath(ids[i])
		if again := resolve(ctx, ids[i]); again != "" && again != path {
			retryFor[i] = again
			retry = append(retry, again)
		}
	}
	if len(retry) > 0 {
		found := fs.statMany(ctx, retry)
		for i, again := range retryFor {
			if st, ok := found[again]; ok {
				// The re-resolved path is what the signature now describes; a
				// retry that also fails keeps the original path's ":absent".
				paths[i] = again
				stats[again] = st
			}
		}
	}

	var b strings.Builder
	for i, path := range paths {
		if path == "" {
			b.WriteString(ids[i])
			b.WriteString(":absent;")
			continue
		}
		st, ok := stats[path]
		if !ok {
			b.WriteString(path)
			b.WriteString(":absent;")
			continue
		}
		fmt.Fprintf(&b, "%s:%d:%d;", path, st.size, st.mod.UnixNano())
	}
	return b.String(), true
}

// chainSig for Claude-style transcripts: stat each id's transcript.
func (fs claudeFS) chainSig(ctx context.Context, ids []string) (string, bool) {
	return statChainSig(ctx, fs, fs.findByID, ids)
}

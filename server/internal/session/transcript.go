package session

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Transcript parses are memoized per file, keyed by the file's size+modtime.
// Claude transcripts are append-only, so a matching stat means a cached parse is
// still current — this avoids re-reading and re-parsing a whole (ever-growing)
// transcript on every attach (LastContextUsage) and history page
// (ReadTranscriptChain). A turn appending to the file changes its size/mtime,
// which invalidates the entry on the next lookup, so no explicit invalidation is
// needed. Entries are keyed by absolute path; the working set is the handful of
// on-disk sessions, so the map stays small.
type transcriptCacheEntry struct {
	size    int64
	modTime time.Time
	msgs    []Message // ReadTranscript's parse; valid only when msgsSet
	msgsSet bool
	snap    *ContextSnapshot // lastUsageInFile's parse; valid only when snapSet
	snapSet bool
}

var (
	transcriptCacheMu sync.Mutex
	transcriptCache   = map[string]transcriptCacheEntry{}
)

// cacheEntryFresh returns the current entry for path if its stat still matches
// (else a zero entry to overwrite), so putters preserve the sibling field
// (msgs vs snap) when only one was (re)computed under the same stat.
func cacheEntryFresh(path string, size int64, mod time.Time) transcriptCacheEntry {
	e := transcriptCache[path]
	if e.size != size || !e.modTime.Equal(mod) {
		return transcriptCacheEntry{size: size, modTime: mod}
	}
	return e
}

func getCachedMsgs(path string, size int64, mod time.Time) ([]Message, bool) {
	transcriptCacheMu.Lock()
	defer transcriptCacheMu.Unlock()
	e, ok := transcriptCache[path]
	if !ok || !e.msgsSet || e.size != size || !e.modTime.Equal(mod) {
		return nil, false
	}
	return e.msgs, true
}

func putCachedMsgs(path string, size int64, mod time.Time, msgs []Message) {
	transcriptCacheMu.Lock()
	defer transcriptCacheMu.Unlock()
	e := cacheEntryFresh(path, size, mod)
	e.msgs, e.msgsSet = msgs, true
	transcriptCache[path] = e
}

func getCachedSnap(path string, size int64, mod time.Time) (*ContextSnapshot, bool) {
	transcriptCacheMu.Lock()
	defer transcriptCacheMu.Unlock()
	e, ok := transcriptCache[path]
	if !ok || !e.snapSet || e.size != size || !e.modTime.Equal(mod) {
		return nil, false
	}
	return e.snap, true
}

func putCachedSnap(path string, size int64, mod time.Time, snap *ContextSnapshot) {
	transcriptCacheMu.Lock()
	defer transcriptCacheMu.Unlock()
	e := cacheEntryFresh(path, size, mod)
	e.snap, e.snapSet = snap, true
	transcriptCache[path] = e
}

// parseTs converts a transcript line's ISO-8601 timestamp to unix seconds,
// returning 0 when it's missing or unparseable.
func parseTs(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// Message is one turn of a session's conversation, extracted from Claude's
// transcript. Role is "user" (what we dictated) or "claude" (the reply). Index
// is the message's position in the filtered conversation (0-based), used as the
// pagination cursor.
type Message struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
	Text  string `json:"text"`
	Ts    int64  `json:"ts"` // unix seconds from the transcript line's timestamp (0 if absent)
	// Usage is the token accounting for a "claude" turn, carried so the per-message
	// context/cache badge survives a reattach or server restart. Set only on the
	// final assistant line of a turn (matching the live badge, which lands on the
	// closing message), and nil on user turns. Omitted from the wire when nil.
	Usage *Usage `json:"usage,omitempty"`
}

// transcriptLine is the subset of a Claude transcript JSONL line we read.
type transcriptLine struct {
	Type      string `json:"type"`      // "user" | "assistant" | (others ignored)
	Timestamp string `json:"timestamp"` // ISO-8601 when Claude Code wrote the line
	Message   struct {
		Content json.RawMessage `json:"content"` // string OR []{type,text,...}
		// Usage is the aggregate token accounting Claude records on each assistant
		// line (Anthropic API field names). Absent on user lines — the zero value.
		Usage struct {
			Input      int `json:"input_tokens"`
			Output     int `json:"output_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

// ContextSnapshot is a session's current on-disk context size: the token usage
// of its most recent assistant turn and when that turn ran (unix seconds), read
// from the transcript so a client can show the context meter — and how much a
// clear/compress would reclaim — immediately on attach, before any live turn.
type ContextSnapshot struct {
	Usage Usage
	At    int64 // unix seconds of the turn (0 if the line had no timestamp)
}

// TranscriptPath locates a session's Claude transcript.
func (s *Session) TranscriptPath() string { return TranscriptPathByID(s.SessionID) }

// TranscriptPathByID finds a LOCAL Claude transcript by session_id (globbed across
// the opaque project-dir encoding). Returns "" if not found. For a specific host,
// go through Driver.claudeFSFor.
func TranscriptPathByID(sessionID string) string { return localClaudeFS.findByID(sessionID) }

// TranscriptPathByID finds a Claude transcript by session_id on the given host
// (empty host = loopback over SSH when SSH-native is wired). Returns "" if absent.
func (d *Driver) TranscriptPathByID(host, sessionID string) string {
	return d.claudeFSFor(host).findByID(sessionID)
}

// TranscriptCwd reads the working directory from a transcript on the given host
// (empty host = loopback over SSH when SSH-native is wired).
func (d *Driver) TranscriptCwd(host, path string) string {
	return d.claudeFSFor(host).transcriptCwd(path)
}

// DeleteSessionsByIDs permanently removes the LOCAL transcript for each given
// session_id (the file <session_id>.jsonl), leaving every OTHER session in the
// same directory untouched. This is how one logical session is deleted — its
// current id plus any rotated prior ids. Returns how many files were removed.
func DeleteSessionsByIDs(ids []string) (int, error) { return localClaudeFS.deleteByIDs(ids) }

// DeleteSessionsForDir permanently removes EVERY LOCAL Claude transcript whose
// working directory is `dir` (legacy whole-directory delete path; per-session
// deletes go through DeleteSessionsByIDs). anySessionID locates the project folder.
func DeleteSessionsForDir(anySessionID, dir string) (int, error) {
	return localClaudeFS.deleteForDir(anySessionID, dir)
}

// ReadTranscript parses a LOCAL transcript JSONL into ordered messages.
func ReadTranscript(path string) ([]Message, error) { return localClaudeFS.readTranscript(path) }

// readTranscript parses a transcript JSONL into ordered user/claude prose messages
// (tool calls, tool results, and metadata lines are skipped) from whichever host
// this claudeFS reads. Returns an empty slice (no error) if the path is empty or
// the file doesn't exist yet.
func (fs claudeFS) readTranscript(path string) ([]Message, error) {
	if path == "" {
		return nil, nil
	}
	key := fs.cacheKey(path)
	size, mod, statOK := fs.stat(path)
	if statOK {
		if m, hit := getCachedMsgs(key, size, mod); hit {
			return append([]Message(nil), m...), nil // copy: callers re-index / mutate Text
		}
	}
	f, err := fs.open(path)
	if err != nil {
		if fs.isMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := newLineScanner(f)
	var out []Message
	idx := 0
	for sc.Scan() {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		var role string
		switch l.Type {
		case "user":
			role = "user"
		case "assistant":
			role = "claude"
		default:
			continue
		}
		text := extractText(l.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue // tool-only turn, tool_result, etc.
		}
		m := Message{Index: idx, Role: role, Text: text, Ts: parseTs(l.Timestamp)}
		if role == "claude" {
			if u := l.Message.Usage; u.Input+u.CacheRead+u.CacheWrite > 0 {
				m.Usage = &Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead}
			}
		}
		out = append(out, m)
		idx++
	}
	// A dictation turn can span several assistant text lines (text interleaved with
	// tool calls); the live badge lands only on the turn's closing message. Match
	// that: keep usage on the last claude line of each run, clearing earlier ones.
	for i := 0; i+1 < len(out); i++ {
		if out[i].Role == "claude" && out[i+1].Role == "claude" {
			out[i].Usage = nil
		}
	}
	if err := sc.Err(); err != nil {
		return out, err // don't cache a partial read
	}
	if statOK {
		putCachedMsgs(key, size, mod, out)
	}
	return out, nil
}

// ReadTranscriptChain reads the transcripts for ids in order (oldest first) and
// concatenates them into one conversation, re-indexing contiguously so the
// pagination cursor (Message.Index) stays stable across the whole chain. Missing
// files — e.g. a freshly-rotated session_id that hasn't run a turn yet —
// contribute nothing. This is how a session "cleared" via context rotation still
// shows its full history even though Claude only ever resumes the newest id.
func ReadTranscriptChain(ids []string) ([]Message, error) {
	return localClaudeFS.readTranscriptChain(ids)
}

// readTranscriptChain is ReadTranscriptChain against whichever host this claudeFS
// reads (local or a remote host over SSH).
func (fs claudeFS) readTranscriptChain(ids []string) ([]Message, error) {
	var all []Message
	for _, id := range ids {
		msgs, err := fs.readTranscript(fs.findByID(id))
		if err != nil {
			return nil, err
		}
		all = append(all, msgs...)
	}
	for i := range all {
		all[i].Index = i
	}
	return all, nil
}

// LastContextUsage returns the context snapshot for a session's transcript
// chain: the most recent assistant turn's token usage (fresh input + cached
// prefix = the live context size) and its timestamp. ids is oldest-first (as
// from TranscriptIDs); the newest transcript carrying a usage-bearing assistant
// line wins. Returns nil when none exists yet (a session that hasn't run a turn).
func LastContextUsage(ids []string) *ContextSnapshot {
	return localClaudeFS.lastContextUsage(ids)
}

// lastContextUsage is LastContextUsage against whichever host this claudeFS reads.
func (fs claudeFS) lastContextUsage(ids []string) *ContextSnapshot {
	for i := len(ids) - 1; i >= 0; i-- {
		if cx := fs.lastUsageInFile(fs.findByID(ids[i])); cx != nil {
			return cx
		}
	}
	return nil
}

// lastUsageInFile scans one transcript for the last assistant line reporting a
// non-zero prompt size, returning its usage + timestamp (nil if none/unreadable).
func (fs claudeFS) lastUsageInFile(path string) *ContextSnapshot {
	if path == "" {
		return nil
	}
	key := fs.cacheKey(path)
	size, mod, statOK := fs.stat(path)
	if statOK {
		if snap, hit := getCachedSnap(key, size, mod); hit {
			return snap
		}
	}
	f, err := fs.open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := newLineScanner(f)
	var last *ContextSnapshot
	for sc.Scan() {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" {
			continue
		}
		u := l.Message.Usage
		if u.Input+u.CacheRead+u.CacheWrite == 0 {
			continue // no aggregate usage on this line (e.g. tool-only sub-turn)
		}
		last = &ContextSnapshot{
			Usage: Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead},
			At:    parseTs(l.Timestamp),
		}
	}
	if err := sc.Err(); err != nil {
		return last // don't cache a partial read
	}
	if statOK {
		putCachedSnap(key, size, mod, last)
	}
	return last
}

// extractText pulls prose from a message.content that may be a plain string
// (user prompts) or an array of blocks (assistant); only "text" blocks count.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}
	return ""
}

// HistoryDigest summarizes a transcript chain for cheap client-cache validation:
// the message count and a content hash over each message (index, role, text,
// timestamp). Two chains producing the same digest are identical as far as the
// app's replayed chat view is concerned, so a client holding a matching digest
// can skip refetching the history entirely. A clear/compress rotation re-indexes
// and rewrites text, so the hash changes — the app sees the mismatch and does a
// full refetch instead of a bad incremental append. The hash is opaque to the
// client: it only ever compares two server-produced hashes for equality.
func HistoryDigest(msgs []Message) (count int, hash string) {
	h := sha256.New()
	var b [8]byte
	for _, m := range msgs {
		binary.BigEndian.PutUint64(b[:], uint64(m.Index))
		h.Write(b[:])
		h.Write([]byte(m.Role))
		h.Write([]byte{0})
		h.Write([]byte(m.Text))
		binary.BigEndian.PutUint64(b[:], uint64(m.Ts))
		h.Write(b[:])
	}
	return len(msgs), hex.EncodeToString(h.Sum(nil))
}

// HistoryPage returns the window of msgs ending just before index `before`
// (exclusive) of size `limit`, plus whether older messages remain. before < 0
// means "from the end" (the most recent page).
func HistoryPage(msgs []Message, before, limit int) (page []Message, more bool) {
	if limit <= 0 {
		limit = 30
	}
	hi := len(msgs)
	if before >= 0 && before < hi {
		hi = before
	}
	lo := hi - limit
	if lo < 0 {
		lo = 0
	}
	return msgs[lo:hi], lo > 0
}

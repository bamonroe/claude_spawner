package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Transcript parses are memoized per file, keyed by the file's size+modtime: a
// matching stat means the cached value is still current, and a turn appending to
// the file changes the stat, so no explicit invalidation is needed. Entries are
// keyed by absolute path (namespaced by host); the working set is the handful of
// on-disk sessions, so the map stays small.
//
// This is the all-or-nothing cache, still used for the context snapshot and by
// the other backends' readers. The Claude message parse itself uses the
// RESUMABLE claudeParseCache below, because invalidating a whole 26 MB parse
// every time a turn appends three lines is precisely the wrong behavior.
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

// claudeParseCache holds the resumable parse per transcript (namespaced by host,
// like transcriptCache). Unlike that map's entries, an entry here stays USEFUL
// after the file changes — that's the whole point: it is the base a later append
// is parsed on top of. Entries are replaced, never invalidated.
var (
	claudeParseMu    sync.Mutex
	claudeParseCache = map[string]incParse{}
)

func getCachedParse(key string) (incParse, bool) {
	claudeParseMu.Lock()
	defer claudeParseMu.Unlock()
	p, ok := claudeParseCache[key]
	return p, ok
}

func putCachedParse(key string, p incParse) {
	claudeParseMu.Lock()
	claudeParseCache[key] = p
	claudeParseMu.Unlock()
}

// incParse is one backend's RESUMABLE parse of an append-only, line-oriented
// transcript. Everything that makes such a parse resumable — statting, deciding
// between an exact hit, an extension and a full re-parse, proving the file really
// was only appended to, and caching the state — is generic, and lives ONCE in
// readIncremental. A backend supplies only the two things that are actually
// backend-specific: how a single line folds into its state, and how that state
// renders to messages.
//
// It exists because "parse only the appended bytes" was originally written for
// Claude alone, so every other backend re-read and re-parsed whole transcripts on
// any change — the reads that matter (history right after a turn, the digest
// sweep) being exactly the ones guaranteed to miss.
type incParse interface {
	// line folds one complete transcript line (no trailing newline) into the state.
	line(raw []byte)
	// state exposes the shared append bookkeeping readIncremental drives.
	state() *appendState
	// cloneParse deep-copies the parse: an extension must never mutate the cached
	// base, which other goroutines may be reading.
	cloneParse() incParse
	// messages renders the parse to a PRIVATE copy — callers re-index and rewrite
	// Text — with any still-open mid-scan state applied to that copy only.
	messages() []Message
}

// appendState is the byte-level bookkeeping every incParse shares: which stat the
// parse corresponds to, how far into the file it got, and the proof-of-append tail.
type appendState struct {
	size int64     // stat at the time of the parse: the exact-hit key
	mod  time.Time //  "
	// parsed is how many bytes of the file the parse covers, always ending on a
	// line boundary — a trailing partial line (a turn mid-write) is left unconsumed
	// so it gets parsed whole on the next read.
	parsed int64
	// overlap is the tail of the parsed region, re-read and compared on every
	// extension to prove the file really was only appended to.
	overlap []byte
}

func (s *appendState) state() *appendState { return s }

// clone copies the state's own byte slice so an extension can't alias the base's.
func (s appendState) clone() appendState {
	s.overlap = append([]byte(nil), s.overlap...)
	return s
}

// advance records that `full` (whole lines only, starting at the parse's current
// offset) has been folded in, and keeps its tail as the next extension's proof.
func (s *appendState) advance(full []byte) {
	s.parsed += int64(len(full))
	if len(full) >= overlapBytes {
		s.overlap = append([]byte(nil), full[len(full)-overlapBytes:]...)
		return
	}
	keep := append(s.overlap, full...)
	if len(keep) > overlapBytes {
		keep = keep[len(keep)-overlapBytes:]
	}
	s.overlap = append([]byte(nil), keep...)
}

// consumeInto folds the next chunk of a file into p. data must begin on a line
// boundary; a trailing partial line is deliberately not consumed.
func consumeInto(p incParse, data []byte) {
	rest := data
	consumed := 0
	for {
		i := bytes.IndexByte(rest, '\n')
		if i < 0 {
			break // partial line: leave it for the next read
		}
		p.line(rest[:i])
		rest = rest[i+1:]
		consumed += i + 1
	}
	p.state().advance(data[:consumed])
}

// readIncremental is THE read path for an append-only, line-oriented transcript,
// shared by every backend whose store is one (Claude, Codex): an exact stat hit
// returns the cached parse, growth parses only the appended bytes on top of it,
// and anything else falls back to a full read. newParse builds an empty state.
//
// Returns (nil, nil) for an empty path or a file that doesn't exist yet, matching
// the "missing file → empty history" convention every reader follows.
func (fs claudeFS) readIncremental(ctx context.Context, path string, newParse func() incParse) ([]Message, error) {
	if path == "" {
		return nil, nil
	}
	key := fs.cacheKey(path)
	size, mod, statOK := fs.stat(ctx, path)
	base, hasBase := getCachedParse(key)
	if statOK && hasBase && base.state().size == size && base.state().mod.Equal(mod) {
		return base.messages(), nil // unchanged since the last parse
	}
	// Grown since the last parse: read and parse only the appended bytes.
	if statOK && hasBase && size > base.state().parsed {
		if st, ok := fs.extendParse(ctx, path, base); ok {
			st.state().size, st.state().mod = size, mod
			putCachedParse(key, st)
			return st.messages(), nil
		}
	}
	data, err := fs.readAll(ctx, path)
	if err != nil {
		if fs.isMissing(err) {
			return nil, nil
		}
		return nil, err
	}
	st := newParse()
	consumeInto(st, data)
	if statOK {
		st.state().size, st.state().mod = size, mod
		putCachedParse(key, st)
	}
	return st.messages(), nil
}

// claudeParse is a RESUMABLE parse of one Claude transcript: the messages
// produced so far, how many bytes of the file they cover, and the mid-scan state
// needed to continue as if the scan had never stopped.
//
// It exists because these transcripts are APPEND-ONLY and ever-growing, while the
// old cache was all-or-nothing on (size, mtime): every turn appended a few lines
// and thereby invalidated the entire cached parse of a file that reaches tens of
// megabytes. So the reads that matter — the history the app asks for right after
// a turn, and the digest sweep — were the ones guaranteed to miss.
type claudeParse struct {
	appendState
	msgs []Message
	// Mid-scan agentic-loop rollup for the dictation currently open: every
	// assistant line counts as one cycle (tool-only ones too, which never become
	// messages), and their usages sum the way the live stream's `result` event
	// does. It is flushed onto the run's last claude message when the NEXT user
	// message starts a new dictation — which may be in a later append, so it stays
	// pending here rather than being baked into msgs.
	idx        int
	curTurns   int
	lastClaude int
	curTotal   Usage
}

// overlapBytes is how much of the already-parsed region an extension re-reads to
// verify the file was only appended to. It rides along in the same read, so the
// only cost is these bytes; a few hundred is plenty to catch a rewrite.
const overlapBytes = 512

// newClaudeParse starts an empty parse (lastClaude = -1: no dictation open yet).
func newClaudeParse() *claudeParse { return &claudeParse{lastClaude: -1} }

// cloneParse copies a parse so an extension never mutates the cached base (which
// other goroutines may be reading).
func (p *claudeParse) cloneParse() incParse {
	c := *p
	c.msgs = append([]Message(nil), p.msgs...)
	c.appendState = p.appendState.clone()
	return &c
}

// line folds one transcript JSONL line into the parse.
func (p *claudeParse) line(raw []byte) {
	var l transcriptLine
	if json.Unmarshal(raw, &l) != nil {
		return
	}
	var role string
	switch l.Type {
	case "user":
		role = "user"
	case "assistant":
		role = "claude"
	default:
		return
	}
	if role == "claude" {
		// Count the cycle before the text filter — a tool-only assistant line is a
		// real API round-trip even though it never becomes a bubble.
		u := l.Message.Usage
		p.curTurns++
		p.curTotal.Input += u.Input
		p.curTotal.Output += u.Output
		p.curTotal.CacheWrite += u.CacheWrite
		p.curTotal.CacheRead += u.CacheRead
	}
	text := extractText(l.Message.Content)
	if strings.TrimSpace(text) == "" {
		return // tool-only turn, tool_result, etc.
	}
	if role == "user" {
		p.flush(p.msgs) // a new dictation begins; close out the previous run
	}
	m := Message{ID: l.UUID, Index: p.idx, Role: role, Text: text, Ts: parseTs(l.Timestamp)}
	if role == "claude" {
		if u := l.Message.Usage; u.Input+u.CacheRead+u.CacheWrite > 0 {
			m.Usage = &Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead}
		}
	}
	p.msgs = append(p.msgs, m)
	if role == "claude" {
		p.lastClaude = len(p.msgs) - 1
	}
	p.idx++
}

// flush writes the open run's rollup onto its closing claude message in `into`
// and resets the counters. `into` is the caller's slice so messages() can flush
// the STILL-OPEN run onto its private copy without disturbing the cached parse.
func (p *claudeParse) flush(into []Message) {
	if p.lastClaude >= 0 && p.curTurns > 0 && p.lastClaude < len(into) {
		total := p.curTotal
		into[p.lastClaude].Turns, into[p.lastClaude].TurnTotal = p.curTurns, &total
	}
	p.curTurns, p.lastClaude, p.curTotal = 0, -1, Usage{}
}

// messages returns the parse's result: a private copy (callers re-index and
// mutate Text) with the still-open dictation's rollup applied. That last flush
// happens on the copy, not the cached state, because a later append can still add
// assistant lines to the same run.
func (p *claudeParse) messages() []Message {
	out := append([]Message(nil), p.msgs...)
	if p.lastClaude >= 0 && p.curTurns > 0 && p.lastClaude < len(out) {
		total := p.curTotal
		out[p.lastClaude].Turns, out[p.lastClaude].TurnTotal = p.curTurns, &total
	}
	// A dictation turn can span several assistant text lines (text interleaved with
	// tool calls); the live badge lands only on the turn's closing message. Match
	// that: keep usage on the last claude line of each run, clearing earlier ones.
	for i := 0; i+1 < len(out); i++ {
		if out[i].Role == "claude" && out[i+1].Role == "claude" {
			out[i].Usage = nil
		}
	}
	return out
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
	Index int `json:"index"`
	// ID is the row's DURABLE identity, read from the backend transcript (Claude
	// JSONL uuid, opencode msg_… id; Codex event lines and Antigravity have none
	// yet, so it's empty there). Unlike Index — which is a positional cursor
	// rewritten on every clear/compress rotation and cross-backend re-index — ID
	// is stable across rotations, so the app reconciles live↔history rows by it
	// (falling back to Index/text when absent). Omitted from the wire when empty.
	ID   string `json:"id,omitempty"`
	Role string `json:"role"`
	Text string `json:"text"`
	Ts   int64  `json:"ts"` // unix seconds from the transcript line's timestamp (0 if absent)
	// Usage is the token accounting for a "claude" turn, carried so the per-message
	// context/cache badge survives a reattach or server restart. Set only on the
	// final assistant line of a turn (matching the live badge, which lands on the
	// closing message), and nil on user turns. Omitted from the wire when nil.
	Usage *Usage `json:"usage,omitempty"`
	// Turns / TurnTotal are the agentic-loop rollup for the dictation this message
	// closes: how many API request/response cycles ran and the tokens they summed
	// to. Live turns carry the same pair on the closing `output` frame (counted from
	// the stream's `assistant` events and the `result` event's aggregate usage);
	// here they're reconstructed from the transcript — every assistant line since
	// the last user message, tool-only ones included — so the detailed badge keeps
	// its "N turns · X total" tail after a reattach instead of losing it to history.
	// Set only where Usage is (the run's closing claude line); omitted when zero.
	Turns     int    `json:"turns,omitempty"`
	TurnTotal *Usage `json:"turn_total,omitempty"`
}

// transcriptLine is the subset of a Claude transcript JSONL line we read.
type transcriptLine struct {
	Type      string `json:"type"`      // "user" | "assistant" | (others ignored)
	UUID      string `json:"uuid"`      // Claude Code's durable per-line id (stable across rotation)
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
func (s *Session) TranscriptPath(ctx context.Context) string {
	return TranscriptPathByID(ctx, s.SessionID)
}

// TranscriptPathByID finds a LOCAL Claude transcript by session_id (globbed across
// the opaque project-dir encoding). Returns "" if not found. For a specific host,
// go through Driver.claudeFSFor.
func TranscriptPathByID(ctx context.Context, sessionID string) string {
	return localClaudeFS.findByID(ctx, sessionID)
}

// TranscriptPathByID finds a Claude transcript by session_id on the given host
// (empty host = loopback over SSH when SSH-native is wired). Returns "" if absent.
func (d *Driver) TranscriptPathByID(ctx context.Context, host, sessionID string) string {
	return d.claudeFSFor(host).findByID(ctx, sessionID)
}

// TranscriptCwd reads the working directory from a transcript on the given host
// (empty host = loopback over SSH when SSH-native is wired).
func (d *Driver) TranscriptCwd(ctx context.Context, host, path string) string {
	return d.claudeFSFor(host).transcriptCwd(ctx, path)
}

// DeleteSessionsByIDs permanently removes the LOCAL transcript for each given
// session_id (the file <session_id>.jsonl), leaving every OTHER session in the
// same directory untouched. This is how one logical session is deleted — its
// current id plus any rotated prior ids. Returns how many files were removed.
func DeleteSessionsByIDs(ctx context.Context, ids []string) (int, error) {
	return localClaudeFS.deleteByIDs(ctx, ids)
}

// DeleteSessionsForDir permanently removes EVERY LOCAL Claude transcript whose
// working directory is `dir` (legacy whole-directory delete path; per-session
// deletes go through DeleteSessionsByIDs). anySessionID locates the project folder.
func DeleteSessionsForDir(ctx context.Context, anySessionID, dir string) (int, error) {
	return localClaudeFS.deleteForDir(ctx, anySessionID, dir)
}

// ReadTranscript parses a LOCAL transcript JSONL into ordered messages.
func ReadTranscript(ctx context.Context, path string) ([]Message, error) {
	return localClaudeFS.readTranscript(ctx, path)
}

// readTranscript parses a transcript JSONL into ordered user/claude prose messages
// (tool calls, tool results, and metadata lines are skipped) from whichever host
// this claudeFS reads. Returns an empty slice (no error) if the path is empty or
// the file doesn't exist yet.
func (fs claudeFS) readTranscript(ctx context.Context, path string) ([]Message, error) {
	return fs.readIncremental(ctx, path, func() incParse { return newClaudeParse() })
}

// readAll returns a transcript's whole contents. (The remote backend's `open`
// already buffers the file in memory, so this costs nothing extra there.)
func (fs claudeFS) readAll(ctx context.Context, path string) ([]byte, error) {
	f, err := fs.open(ctx, path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// extendParse continues a cached parse over the bytes appended since it ran,
// returning the extended state. ok is false when the extension can't be trusted
// (the overlap check failed, or the read errored) and the caller must re-parse
// the whole file.
func (fs claudeFS) extendParse(ctx context.Context, path string, base incParse) (incParse, bool) {
	bs := base.state()
	off := bs.parsed - int64(len(bs.overlap))
	data, err := fs.readFrom(ctx, path, off)
	if err != nil || int64(len(data)) < int64(len(bs.overlap)) {
		return nil, false
	}
	// The append-only invariant is an assumption about how Claude Code writes, not
	// something we control — a rewritten or truncated-and-replaced transcript would
	// otherwise be silently mis-parsed. Re-reading a little of the ALREADY-parsed
	// region and checking it byte-for-byte is what makes the assumption verified
	// rather than trusted, and it rides along in the same read, so it's free.
	if !bytes.Equal(data[:len(bs.overlap)], bs.overlap) {
		return nil, false
	}
	st := base.cloneParse()
	consumeInto(st, data[len(bs.overlap):])
	return st, true
}

// readFrom returns a transcript's bytes from off to EOF.
func (fs claudeFS) readFrom(ctx context.Context, path string, off int64) ([]byte, error) {
	if off <= 0 {
		return fs.readAll(ctx, path)
	}
	if fs.remote == nil {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if _, err := f.Seek(off, io.SeekStart); err != nil {
			return nil, err
		}
		return io.ReadAll(f)
	}
	// `tail -c +N` is 1-based on the byte offset.
	out, err := fs.remote.output(ctx, fmt.Sprintf("tail -c +%d %s", off+1, shellQuote(path)))
	if err != nil {
		return nil, errRemoteMissing
	}
	return out, nil
}

// ReadTranscriptChain reads the transcripts for ids in order (oldest first) and
// concatenates them into one conversation, re-indexing contiguously so the
// pagination cursor (Message.Index) stays stable across the whole chain. Missing
// files — e.g. a freshly-rotated session_id that hasn't run a turn yet —
// contribute nothing. This is how a session "cleared" via context rotation still
// shows its full history even though Claude only ever resumes the newest id.
func ReadTranscriptChain(ctx context.Context, ids []string) ([]Message, error) {
	return localClaudeFS.readTranscriptChain(ctx, ids)
}

// readTranscriptChain is ReadTranscriptChain against whichever host this claudeFS
// reads (local or a remote host over SSH).
func (fs claudeFS) readTranscriptChain(ctx context.Context, ids []string) ([]Message, error) {
	var all []Message
	for _, id := range ids {
		path := fs.findByID(ctx, id)
		msgs, err := fs.readTranscript(ctx, path)
		if err != nil {
			return nil, err
		}
		// Nothing there can mean the memoized path went stale (the transcript moved
		// or was replaced out of band) rather than "this id has no messages yet".
		// Re-resolve once before accepting the empty answer; a genuinely empty
		// transcript is cheap to re-check and rare.
		if len(msgs) == 0 && path != "" {
			fs.forgetPath(id)
			if again := fs.findByID(ctx, id); again != path {
				if msgs, err = fs.readTranscript(ctx, again); err != nil {
					return nil, err
				}
			}
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
func LastContextUsage(ctx context.Context, ids []string) *ContextSnapshot {
	return localClaudeFS.lastContextUsage(ctx, ids)
}

// lastContextUsage is LastContextUsage against whichever host this claudeFS reads.
func (fs claudeFS) lastContextUsage(ctx context.Context, ids []string) *ContextSnapshot {
	for i := len(ids) - 1; i >= 0; i-- {
		if cx := fs.lastUsageInFile(ctx, fs.findByID(ctx, ids[i])); cx != nil {
			return cx
		}
	}
	return nil
}

// usageTailBudgets are the byte windows lastUsageInFile reads from the END of a
// transcript, smallest first, until it finds a usage-bearing assistant line.
//
// The answer is almost always in the final line or two, so the first window
// settles it; the escalation only pays off on a transcript whose tail is a run of
// enormous tool-result lines. A single line CAN be megabytes, which is why these
// are byte budgets rather than line counts — the same reasoning as cwdHeadBytes,
// from the other end of the file.
var usageTailBudgets = []int64{256 << 10, 4 << 20, 32 << 20}

// lastUsageInFile finds the last assistant line reporting a non-zero prompt size,
// returning its usage + timestamp (nil if none/unreadable). It reads BACKWARD over
// a bounded tail rather than scanning the whole file forward: this runs at the end
// of every turn (the context meter) and on every attach, against a file the turn
// just appended to — so the parse cache never helps and the full read was paid in
// full, every time.
func (fs claudeFS) lastUsageInFile(ctx context.Context, path string) *ContextSnapshot {
	if path == "" {
		return nil
	}
	key := fs.cacheKey(path)
	size, mod, statOK := fs.stat(ctx, path)
	if statOK {
		if snap, hit := getCachedSnap(key, size, mod); hit {
			return snap
		}
	}
	for _, budget := range usageTailBudgets {
		data, whole, err := fs.tailBytes(ctx, path, budget)
		if err != nil {
			return nil
		}
		if !whole {
			// The window opened mid-line; that fragment isn't parseable JSON, and
			// an earlier complete copy of it is out of reach anyway.
			if _, rest, ok := bytes.Cut(data, []byte("\n")); ok {
				data = rest
			} else {
				data = nil // one partial line filled the whole window
			}
		}
		snap := lastUsageInLines(data)
		if snap == nil && !whole {
			continue // nothing in this window; widen it
		}
		if statOK {
			putCachedSnap(key, size, mod, snap)
		}
		return snap
	}
	return nil
}

// lastUsageInLines scans a block of transcript lines from the END, returning the
// first (i.e. latest) assistant line that carries aggregate token usage.
func lastUsageInLines(data []byte) *ContextSnapshot {
	for len(data) > 0 {
		line := data
		if i := bytes.LastIndexByte(data, '\n'); i >= 0 {
			line, data = data[i+1:], data[:i]
		} else {
			data = nil
		}
		var l transcriptLine
		if json.Unmarshal(line, &l) != nil || l.Type != "assistant" {
			continue
		}
		u := l.Message.Usage
		if u.Input+u.CacheRead+u.CacheWrite == 0 {
			continue // no aggregate usage on this line (e.g. tool-only sub-turn)
		}
		return &ContextSnapshot{
			Usage: Usage{Input: u.Input, Output: u.Output, CacheWrite: u.CacheWrite, CacheRead: u.CacheRead},
			At:    parseTs(l.Timestamp),
		}
	}
	return nil
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
// the message count and a content hash over each message (durable id or position,
// role, text, timestamp). Two chains producing the same digest are identical as far
// as the app's replayed chat view is concerned, so a client holding a matching
// digest can skip refetching the history entirely. Each row is keyed by its durable
// ID when it has one, so the hash is stable across a clear/compress rotation or a
// cross-backend re-index that only shifts positions — the app then skips a needless
// refetch; id-less rows fall back to the positional Index (today's behavior). A
// rotation that actually changes content or count still flips the digest, so the
// app refetches when it must. The hash is opaque to the client: it only ever
// compares two server-produced hashes for equality.
func HistoryDigest(msgs []Message) (count int, hash string) {
	h := sha256.New()
	var b [8]byte
	for _, m := range msgs {
		if m.ID != "" {
			h.Write([]byte(m.ID))
		} else {
			binary.BigEndian.PutUint64(b[:], uint64(m.Index))
			h.Write(b[:])
		}
		h.Write([]byte{0})
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

package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Antigravity reply reconstruction — restoring the paragraph/message breaks that
// `agy --print` throws away.
//
// agy runs one non-interactive turn and prints the final response as plain prose
// on stdout, but when a turn emits several assistant messages (e.g. "building…",
// then "hit a compile error, fixed it", then "build passed") it flushes them to
// stdout as one space-joined blob with no line breaks — agy has no structured or
// JSON stdout mode. parseAgyText faithfully forwards that blob, so the client
// renders a whole turn as a single wall-of-text paragraph.
//
// agy does record each assistant message as its own PLANNER_RESPONSE record in a
// per-conversation transcript.jsonl under
// ~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/. reconstructAgyReply
// reads that transcript back and rejoins the messages with blank lines, restoring
// the breaks the stdout blob lost.
//
// Locating the right transcript: agy IGNORES the --conversation id we pass (it logs
// "not found, ignoring --conversation flag" and keys its store by an internal id of
// its own), so we cannot look the file up by our session id the way the Claude
// reader does. Instead we read the few most-recently-written brain transcripts on
// the same target agy ran on and pick the one whose messages, joined by spaces,
// reproduce agy's stdout blob exactly (whitespace-normalized). That content match
// doubles as the safety guard: we only ever rewrite line breaks, never wording, and
// fall back to the original stdout reply on any miss, mismatch, or error.
//
// (The ignored --conversation id also means agy is not actually resuming our
// conversations turn-to-turn. To recover a session's history despite that, we CAPTURE
// each turn's brain id here — reconstructAgyReply returns the id of the matched dir,
// and the caller records it on the session (Session.AgyBrainIDs), building our own
// ordered map from a session to agy's on-disk turns for the history reader to replay.
// antigravityFS below is that history reader: driver.transcriptReaderFor hands it the
// AgyBrainIDs chain and it replays each brain's transcript.jsonl as one user row plus
// one joined assistant row per turn.)

// agyBrainScript lists the newest brain transcripts on the target (newest first)
// and, for each, emits a marker line carrying the transcript's path followed by only
// its PLANNER_RESPONSE records. The path lets the caller recover the brain dir id of
// the block it matched. The type filter keeps the payload small — a transcript's
// bulky tool-result lines (embedded file dumps) never leave the target. $HOME and
// $() expand in the target shell; RunOnTarget runs the command via sh -c on host,
// SSH, and sandbox alike.
const agyBrainScript = `for f in $(ls -1dt "$HOME"/.gemini/antigravity-cli/brain/*/.system_generated/logs/transcript.jsonl 2>/dev/null | head -6); do echo "@@AGY@@ $f"; grep -F '"PLANNER_RESPONSE"' "$f" 2>/dev/null; done`

// agyMarker separates one transcript's PLANNER_RESPONSE lines from the next in the
// script's combined output.
const agyMarker = "@@AGY@@"

var agyWSRun = regexp.MustCompile(`\s+`)

// agyCollapseWS normalizes all whitespace runs to single spaces so the stdout blob
// and the space-joined transcript messages compare equal regardless of incidental
// spacing differences.
func agyCollapseWS(s string) string {
	return strings.TrimSpace(agyWSRun.ReplaceAllString(s, " "))
}

// agyTranscriptLine is the subset of a brain transcript.jsonl record we read.
type agyTranscriptLine struct {
	StepIndex int             `json:"step_index"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"created_at"` // RFC3339 when agy wrote the step (history reader)
	Content   json.RawMessage `json:"content"`    // a string for a spoken message; null/absent for tool-only steps
}

// reconstructAgyReply returns flat re-broken into paragraphs when it can find and
// verify the transcript agy just wrote, else flat unchanged, plus the brain dir id
// of the matched transcript ("" on any miss). flat is parseAgyText's stdout reply —
// both the fallback and the correctness key; the id lets the caller record which
// on-disk turn this reply came from (Session.AgyBrainIDs).
func (d *Driver) reconstructAgyReply(ctx context.Context, s *Session, flat string) (string, string) {
	want := agyCollapseWS(flat)
	if want == "" {
		return flat, ""
	}
	out, err := d.RunOnTarget(ctx, s, agyBrainScript)
	if err != nil {
		return flat, ""
	}
	if para, id, ok := matchAgyParagraphs(string(out), want); ok {
		return para, id
	}
	return flat, ""
}

// matchAgyParagraphs scans the brain-script output (marker-separated transcript
// blocks, newest first) for the block whose PLANNER_RESPONSE messages, joined by
// spaces, reproduce want (an already-whitespace-collapsed stdout blob). On a match
// it returns those messages rejoined with blank lines and the matched block's brain
// dir id (parsed from the marker's path payload; "" if unrecognizable); otherwise ok
// is false and the caller keeps the original stdout reply.
func matchAgyParagraphs(out, want string) (string, string, bool) {
	for _, block := range strings.Split(out, agyMarker) {
		msgs := agyPlannerMessages(block)
		if len(msgs) == 0 {
			continue
		}
		if agyCollapseWS(strings.Join(msgs, " ")) == want {
			return strings.Join(msgs, "\n\n"), agyBrainID(block), true
		}
	}
	return "", "", false
}

// agyBrainID extracts the brain dir id (the <id> in
// .../brain/<id>/.system_generated/logs/transcript.jsonl) from a block's leading
// marker-payload line — the transcript path agyBrainScript emits after each @@AGY@@.
// Returns "" if the block has no recognizable path line (e.g. a synthetic test block
// whose marker carried no path).
func agyBrainID(block string) string {
	const seg = "/brain/"
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "{") {
			continue // skip blanks and the PLANNER_RESPONSE json lines
		}
		i := strings.Index(line, seg)
		if i < 0 {
			return "" // first non-json line isn't a brain path
		}
		rest := line[i+len(seg):]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			return rest[:j]
		}
		return ""
	}
	return ""
}

// agyPlannerMessages parses one transcript block's PLANNER_RESPONSE lines into the
// ordered, non-empty assistant message texts (by step index). Lines that fail to
// parse, aren't PLANNER_RESPONSE, or whose content isn't a non-empty string (a
// tool-only planner step carries null content) are skipped.
func agyPlannerMessages(block string) []string {
	type msg struct {
		step int
		text string
	}
	var msgs []msg
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var l agyTranscriptLine
		if json.Unmarshal([]byte(line), &l) != nil || l.Type != "PLANNER_RESPONSE" {
			continue
		}
		var text string
		if json.Unmarshal(l.Content, &text) != nil || strings.TrimSpace(text) == "" {
			continue
		}
		msgs = append(msgs, msg{l.StepIndex, strings.TrimSpace(text)})
	}
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].step < msgs[j].step })
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.text
	}
	return out
}

// antigravityFS is the transcriptReader for antigravity sessions. agy ignores our
// --conversation id and keys its store by internal brain-dir ids, which we capture
// per turn in Session.AgyBrainIDs (see reconstructAgyReply). transcriptReaderFor
// hands that chain to readTranscriptChain, so each id resolves to one brain's
// transcript.jsonl and replays as one user row + one joined assistant row — the same
// paragraph-join reconstructAgyReply applies to the live reply, so a replayed turn
// dedupes against its live copy. Each row carries a durable id (brain dir + role),
// stable across a re-index. It embeds claudeFS purely for the local-vs-SSH file
// primitives (stat/open/cacheKey/remote); layout and parsing are agy-specific.
//
// Like the Codex and opencode readers, it reads over the host/SSH filesystem, so a
// turn that ran inside a sandbox container (whose brain dir lives in the container)
// is not visible here — that's the pre-existing reader limitation, not agy-specific.
type antigravityFS struct {
	claudeFS
}

// agyTranscriptRel is the path of a brain's transcript.jsonl relative to the brain
// dir id, under ~/.gemini/antigravity-cli/brain/<id>/.
const agyTranscriptRel = ".system_generated/logs/transcript.jsonl"

// findByID returns the transcript.jsonl path for an antigravity brain dir id, or ""
// if the id is empty / unsafe. Overrides claudeFS.findByID (which globs Claude's
// projects tree). The path is deterministic (no glob) — the id IS the directory.
func (fs antigravityFS) findByID(brainID string) string {
	if brainID == "" || !looksLikeUUID(brainID) {
		return "" // guard: the id is interpolated into a path (and a remote command)
	}
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, ".gemini", "antigravity-cli", "brain", brainID, agyTranscriptRel)
	}
	return `$HOME/.gemini/antigravity-cli/brain/` + brainID + `/` + agyTranscriptRel
}

// readTranscriptChain concatenates the brain transcripts for ids (oldest first, the
// turn order AgyBrainIDs records) into one re-indexed conversation. Mirrors
// claudeFS.readTranscriptChain against agy brains (embedding gives no virtual
// dispatch, so it binds to antigravityFS's readTranscript override here).
func (fs antigravityFS) readTranscriptChain(ids []string) ([]Message, error) {
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

// lastContextUsage reports no snapshot: agy records no token accounting in its
// transcript, so an agy session simply has no context badge. Overrides the embedded
// Claude scanner.
func (fs antigravityFS) lastContextUsage(ids []string) *ContextSnapshot { return nil }

// deleteByIDs is a deliberate no-op: the brain dirs are agy's own store (it may reuse
// or reconcile them), and history replay only borrows them, so a spawner-side session
// delete leaves them untouched.
func (fs antigravityFS) deleteByIDs(ids []string) (int, error) { return 0, nil }

// readTranscript parses one brain transcript.jsonl into a single turn: the user's
// request (USER_INPUT, unwrapped from agy's <USER_REQUEST> envelope; our own prompt
// scaffolding is stripped later by the gateway, as for every backend) followed by the
// joined assistant reply (PLANNER_RESPONSE steps rejoined with blank lines, exactly as
// reconstructAgyReply renders the live reply). Empty path / missing file yields an
// empty slice (no error), matching claudeFS.readTranscript.
func (fs antigravityFS) readTranscript(path string) ([]Message, error) {
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

	brainID := agyBrainDirFromPath(path)
	var userMsg *Message
	type replyStep struct {
		step int
		text string
		ts   int64
	}
	var replies []replyStep
	sc := newLineScanner(f)
	for sc.Scan() {
		var l agyTranscriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "USER_INPUT":
			var raw string
			if json.Unmarshal(l.Content, &raw) != nil {
				continue
			}
			text := agyUnwrapUserRequest(raw)
			if strings.TrimSpace(text) == "" {
				continue
			}
			userMsg = &Message{ID: brainID + ":in", Role: "user", Text: text, Ts: parseTs(l.CreatedAt)}
		case "PLANNER_RESPONSE":
			var text string
			if json.Unmarshal(l.Content, &text) != nil || strings.TrimSpace(text) == "" {
				continue
			}
			replies = append(replies, replyStep{l.StepIndex, strings.TrimSpace(text), parseTs(l.CreatedAt)})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err // don't cache a partial read
	}

	var out []Message
	if userMsg != nil {
		out = append(out, *userMsg)
	}
	if len(replies) > 0 {
		sort.SliceStable(replies, func(i, j int) bool { return replies[i].step < replies[j].step })
		texts := make([]string, len(replies))
		for i, r := range replies {
			texts[i] = r.text
		}
		out = append(out, Message{
			ID:   brainID + ":resp",
			Role: "claude",
			Text: strings.Join(texts, "\n\n"),
			Ts:   replies[len(replies)-1].ts, // the turn's last spoken step
		})
	}
	if statOK {
		putCachedMsgs(key, size, mod, out)
	}
	return out, nil
}

// agyUnwrapUserRequest returns the text inside agy's <USER_REQUEST>…</USER_REQUEST>
// envelope (which also carries our own appended scaffolding, stripped downstream by
// the gateway's stripInjected like every backend). If the envelope is absent it
// returns the raw content unchanged — best-effort, never lossy.
func agyUnwrapUserRequest(s string) string {
	const open, close = "<USER_REQUEST>", "</USER_REQUEST>"
	i := strings.Index(s, open)
	if i < 0 {
		return s
	}
	rest := s[i+len(open):]
	if j := strings.Index(rest, close); j >= 0 {
		return strings.TrimSpace(rest[:j])
	}
	return strings.TrimSpace(rest)
}

// agyBrainDirFromPath extracts the brain dir id from a transcript path
// (.../brain/<id>/.system_generated/logs/transcript.jsonl), the anchor for each row's
// durable id. Returns "" if the path has no recognizable /brain/<id>/ segment.
func agyBrainDirFromPath(path string) string {
	const seg = "/brain/"
	i := strings.Index(path, seg)
	if i < 0 {
		return ""
	}
	rest := path[i+len(seg):]
	if j := strings.IndexByte(rest, '/'); j >= 0 {
		return rest[:j]
	}
	return ""
}

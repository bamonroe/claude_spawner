package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Antigravity history: replaying agy's on-disk brain transcripts.
//
// agy keys its store by a conversation id it mints itself. That id is announced in
// the turn stream's init event (agent.parseAgyStream adopts it as the session id)
// and IS the name of the on-disk brain directory, so a session's turns are found
// directly by id — no content matching needed. The driver records each turn's id in
// Session.AgyBrainIDs (usually a single entry; a session grows a second only if a
// resume failed and agy minted a fresh conversation).
//
// antigravityFS below is the history reader: driver.transcriptReaderFor hands it
// that chain and it replays each brain's
// ~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript.jsonl as
// one user row plus one joined assistant row per turn.

// agyTranscriptLine is the subset of a brain transcript.jsonl record we read.
type agyTranscriptLine struct {
	StepIndex int             `json:"step_index"`
	Type      string          `json:"type"`
	CreatedAt string          `json:"created_at"` // RFC3339 when agy wrote the step (history reader)
	Content   json.RawMessage `json:"content"`    // a string for a spoken message; null/absent for tool-only steps
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

// chainSig stats the brain transcripts. Declared explicitly for the same reason
// as codexFS.chainSig: the inherited claudeFS method would stat the wrong paths.
func (fs antigravityFS) chainSig(ids []string) (string, bool) {
	return statChainSig(fs.claudeFS, fs.findByID, ids)
}

package session

import (
	"context"
	"encoding/json"
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
// The TranscriptAntigravity history reader that consumes that map is still a follow-up
// in TODO.md.)

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
	Content   json.RawMessage `json:"content"` // a string for a spoken message; null/absent for tool-only steps
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

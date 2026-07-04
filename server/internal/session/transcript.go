package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

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
}

// transcriptLine is the subset of a Claude transcript JSONL line we read.
type transcriptLine struct {
	Type      string `json:"type"`      // "user" | "assistant" | (others ignored)
	Timestamp string `json:"timestamp"` // ISO-8601 when Claude Code wrote the line
	Message   struct {
		Content json.RawMessage `json:"content"` // string OR []{type,text,...}
	} `json:"message"`
}

// TranscriptPath locates a session's Claude transcript.
func (s *Session) TranscriptPath() string { return TranscriptPathByID(s.SessionID) }

// TranscriptPathByID finds a Claude transcript by session_id. The file is named
// <session_id>.jsonl under ~/.claude/projects/<encoded-dir>/, but the dir
// encoding is fiddly — the session_id is globally unique, so we glob for it.
// Returns "" if not found.
func TranscriptPathByID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
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

// DeleteSessionsForDir permanently removes EVERY Claude transcript whose working
// directory is `dir` — because Discover shows one entry per directory, deleting
// that entry should clear all of the directory's sessions (otherwise the entry
// reappears with the next-newest session, looking like a failed delete).
// anySessionID is any session known to live in that dir, used to locate the
// project folder. Returns how many transcripts were deleted.
func DeleteSessionsForDir(anySessionID, dir string) (int, error) {
	path := TranscriptPathByID(anySessionID)
	if path == "" {
		return 0, nil
	}
	// All sessions for a given cwd live in the same ~/.claude/projects/<enc>/
	// folder; match on cwd too, in case two paths encode to the same folder.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.jsonl"))
	n := 0
	for _, f := range matches {
		if TranscriptCwd(f) != dir {
			continue
		}
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return n, err
		}
		n++
	}
	return n, nil
}

// ReadTranscript parses a transcript JSONL into ordered user/claude prose
// messages (tool calls, tool results, and metadata lines are skipped). Returns
// an empty slice (no error) if the path is empty or the file doesn't exist yet.
func ReadTranscript(path string) ([]Message, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
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
		out = append(out, Message{Index: idx, Role: role, Text: text, Ts: parseTs(l.Timestamp)})
		idx++
	}
	return out, sc.Err()
}

// ReadTranscriptChain reads the transcripts for ids in order (oldest first) and
// concatenates them into one conversation, re-indexing contiguously so the
// pagination cursor (Message.Index) stays stable across the whole chain. Missing
// files — e.g. a freshly-rotated session_id that hasn't run a turn yet —
// contribute nothing. This is how a session "cleared" via context rotation still
// shows its full history even though Claude only ever resumes the newest id.
func ReadTranscriptChain(ids []string) ([]Message, error) {
	var all []Message
	for _, id := range ids {
		msgs, err := ReadTranscript(TranscriptPathByID(id))
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

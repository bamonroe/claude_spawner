package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Message is one turn of a session's conversation, extracted from Claude's
// transcript. Role is "user" (what we dictated) or "claude" (the reply). Index
// is the message's position in the filtered conversation (0-based), used as the
// pagination cursor.
type Message struct {
	Index int    `json:"index"`
	Role  string `json:"role"`
	Text  string `json:"text"`
}

// transcriptLine is the subset of a Claude transcript JSONL line we read.
type transcriptLine struct {
	Type    string `json:"type"` // "user" | "assistant" | (others ignored)
	Message struct {
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

// DeleteTranscript permanently removes a Claude session's transcript from disk
// (by session_id). After this, `claude --resume <id>` no longer works — the
// conversation is gone. Returns nil if there was nothing to delete.
func DeleteTranscript(sessionID string) error {
	path := TranscriptPathByID(sessionID)
	if path == "" {
		return nil
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
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

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24) // tool inputs can exceed 64KB
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
		out = append(out, Message{Index: idx, Role: role, Text: text})
		idx++
	}
	return out, sc.Err()
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

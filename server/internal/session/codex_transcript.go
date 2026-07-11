package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// codexFS reads a Codex session's on-disk state so its past conversation replays
// on reattach, exactly like a Claude session's. Codex persists each session as a
// "rollout" JSONL under ~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl
// (the thread_id — Codex's session id — is the trailing UUID in the filename), in
// a schema unrelated to Claude's transcript OR to Codex's live `codex exec --json`
// stream: conversation prose arrives as `event_msg` lines of type `user_message`
// and `agent_message`, and context size as `token_count` lines. This reads that
// persisted schema; the live stream is handled by parseCodexStream.
//
// It embeds claudeFS purely to reuse the backend-neutral file primitives
// (stat/open/isMissing/cacheKey and the local-vs-SSH split) — only where the
// files live (findByID) and how they parse differ, so those are overridden here.
type codexFS struct {
	claudeFS
}

// findByID returns the rollout path for a Codex thread_id (the trailing UUID in
// the rollout filename), globbed across the date-partitioned session tree, or ""
// if not found. Overrides claudeFS.findByID (which globs ~/.claude/projects).
func (fs codexFS) findByID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		matches, _ := filepath.Glob(filepath.Join(home, ".codex", "sessions", "*", "*", "*", "rollout-*-"+sessionID+".jsonl"))
		if len(matches) > 0 {
			return matches[0]
		}
		return ""
	}
	if !looksLikeUUID(sessionID) {
		return "" // guard the value we interpolate into the remote glob
	}
	out, err := fs.remote.output(`ls -1d "$HOME/.codex/sessions/"*/*/*/rollout-*-` + sessionID + `.jsonl 2>/dev/null || true`)
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

// deleteByIDs removes each Codex thread's rollout transcript. Codex keeps the
// whole session in that one rollout .jsonl — no projects-style sidecar or
// per-session state dirs like Claude — so removing it leaves nothing behind.
// Overrides claudeFS.deleteByIDs (embedding gives no virtual dispatch, and the
// Claude path glob wouldn't find a rollout under ~/.codex).
func (fs codexFS) deleteByIDs(ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		p := fs.findByID(id)
		if p == "" {
			continue
		}
		if err := fs.remove(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// readTranscriptChain concatenates the rollout transcripts for ids (oldest first)
// into one re-indexed conversation, mirroring claudeFS.readTranscriptChain but
// against Codex's rollout files and parser. (Embedding gives no virtual dispatch,
// so the chain/usage helpers are restated here to bind to codexFS's overrides.)
func (fs codexFS) readTranscriptChain(ids []string) ([]Message, error) {
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

// lastContextUsage returns the newest transcript's last context snapshot (the most
// recent turn's token_count), scanning ids newest-first. Mirrors
// claudeFS.lastContextUsage against Codex rollouts.
func (fs codexFS) lastContextUsage(ids []string) *ContextSnapshot {
	for i := len(ids) - 1; i >= 0; i-- {
		if cx := fs.lastUsageInFile(fs.findByID(ids[i])); cx != nil {
			return cx
		}
	}
	return nil
}

// codexRolloutLine is the envelope of a Codex rollout JSONL line: a top-level type
// + timestamp wrapping a type-specific payload we decode lazily.
type codexRolloutLine struct {
	Type      string          `json:"type"`      // "event_msg" | "response_item" | "session_meta" | ...
	Timestamp string          `json:"timestamp"` // ISO-8601 when Codex wrote the line
	Payload   json.RawMessage `json:"payload"`
}

// codexEventPayload is the subset of an `event_msg` payload we read: the prose of
// user/agent messages and the token accounting of a completed turn.
type codexEventPayload struct {
	Type    string `json:"type"`    // "user_message" | "agent_message" | "token_count" | ...
	Message string `json:"message"` // prose on user_message / agent_message
	Info    struct {
		// LastTokenUsage is the just-completed turn's usage — the full prompt Codex
		// sent that turn, i.e. the current context occupancy (TotalTokenUsage is a
		// running sum across turns, which is not what a context meter wants).
		LastTokenUsage codexTokenUsage `json:"last_token_usage"`
	} `json:"info"` // on token_count
}

// codexTokenUsage is Codex's per-turn token accounting. InputTokens is the whole
// prompt for the turn and is inclusive of CachedInputTokens (the cached prefix).
type codexTokenUsage struct {
	InputTokens         int `json:"input_tokens"`
	CachedInputTokens   int `json:"cached_input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	ReasoningOutputToks int `json:"reasoning_output_tokens"`
}

// usage maps Codex's token accounting onto our Usage, matching Claude's split:
// Input is the fresh (non-cached) prompt, CacheRead the cached prefix, so
// Input+CacheRead is the full context size. Codex reports no separate cache-write.
func (u codexTokenUsage) usage() Usage {
	fresh := u.InputTokens - u.CachedInputTokens
	if fresh < 0 {
		fresh = 0
	}
	return Usage{
		Input:      fresh,
		Output:     u.OutputTokens + u.ReasoningOutputToks,
		CacheRead:  u.CachedInputTokens,
		CacheWrite: 0,
	}
}

// readTranscript parses a Codex rollout JSONL into ordered user/claude prose
// messages, attaching each turn's token_count usage to its agent_message so the
// per-message context badge survives a reattach. Empty path / missing file yields
// an empty slice (no error), matching claudeFS.readTranscript. Overrides the
// embedded claudeFS parser (which expects Claude's schema).
func (fs codexFS) readTranscript(path string) ([]Message, error) {
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
	idx, lastClaude := 0, -1
	for sc.Scan() {
		var l codexRolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "event_msg" {
			continue
		}
		var p codexEventPayload
		if json.Unmarshal(l.Payload, &p) != nil {
			continue
		}
		switch p.Type {
		case "user_message", "agent_message":
			role := "user"
			if p.Type == "agent_message" {
				role = "claude"
			}
			if strings.TrimSpace(p.Message) == "" {
				continue
			}
			out = append(out, Message{Index: idx, Role: role, Text: p.Message, Ts: parseTs(l.Timestamp)})
			if role == "claude" {
				lastClaude = len(out) - 1
			}
			idx++
		case "token_count":
			// The turn's usage lands after its agent_message; badge that message.
			if lastClaude >= 0 {
				u := p.Info.LastTokenUsage.usage()
				if u.Input+u.CacheRead > 0 {
					out[lastClaude].Usage = &u
				}
			}
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

// lastUsageInFile scans one rollout for the last token_count line, returning its
// context snapshot (nil if none/unreadable). Overrides the embedded Claude parser.
func (fs codexFS) lastUsageInFile(path string) *ContextSnapshot {
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
		var l codexRolloutLine
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "event_msg" {
			continue
		}
		var p codexEventPayload
		if json.Unmarshal(l.Payload, &p) != nil || p.Type != "token_count" {
			continue
		}
		u := p.Info.LastTokenUsage.usage()
		if u.Input+u.CacheRead == 0 {
			continue
		}
		last = &ContextSnapshot{Usage: u, At: parseTs(l.Timestamp)}
	}
	if err := sc.Err(); err != nil {
		return last // don't cache a partial read
	}
	if statOK {
		putCachedSnap(key, size, mod, last)
	}
	return last
}

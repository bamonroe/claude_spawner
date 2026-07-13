package session

import (
	"encoding/json"
	"os/exec"
	"strings"
)

// opencodeFS reads an opencode session's past conversation so it replays on
// reattach, exactly like a Claude or Codex session. Unlike those, opencode does
// NOT persist sessions as flat JSONL files — it keeps them in a SQLite database
// (~/.local/share/opencode/opencode.db). Rather than open that DB (a new
// dependency, coupled to opencode's internal schema), this reader shells out to
// opencode's own stable commands — `opencode export <id>` for history and
// `opencode session delete <id>` for removal — parsing the exported JSON. The
// commands run on the session's host over the same SSH seam claudeFS uses (or
// locally when remote is nil, for the hermetic tests), so this embeds claudeFS
// purely to reuse that local-vs-remote plumbing.
//
// The live turn stream is handled by parseOpencodeStream (agent/opencode.go);
// this is the persisted-history counterpart, mapping opencode's exported
// message/part shape onto the backend-neutral Message/ContextSnapshot model.
type opencodeFS struct {
	claudeFS
}

// opencodeReaderBin is the opencode binary the reader invokes for export/delete.
// It mirrors the SPAWNER_SSH_OPENCODE_BIN default; the transcript readers carry
// no config handle, so a non-default binary name isn't honored here (a known
// limitation — the reader assumes "opencode" is on the host's PATH).
const opencodeReaderBin = "opencode"

// validOpencodeID reports whether id is a well-formed opencode session id
// (`ses_` + alphanumerics). It gates every id before it's interpolated into a
// remote shell command, so a malformed/hostile id can't inject shell.
func validOpencodeID(id string) bool {
	if !strings.HasPrefix(id, "ses_") || len(id) <= len("ses_") {
		return false
	}
	for _, r := range id[len("ses_"):] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// run executes an opencode subcommand on the session's host and returns its
// stdout. Remote (production; SSH-native is unconditional) goes over the pooled
// connection as a single shell line; a nil remote (hermetic tests) runs the
// binary locally. Callers pass only fixed verbs plus an id already vetted by
// validOpencodeID, so the plain space-join used for the remote command is safe.
func (fs opencodeFS) run(args ...string) ([]byte, error) {
	if fs.remote == nil {
		return exec.Command(opencodeReaderBin, args...).Output()
	}
	return fs.remote.output(opencodeReaderBin + " " + strings.Join(args, " "))
}

// opencodeExport is the subset of `opencode export <id>` JSON we read: the
// ordered messages, each a role plus a list of parts. Text parts carry prose;
// step-finish parts carry that step's token accounting. info.tokens (the
// session-level summary) is deliberately ignored — it is SUMMED across turns, so
// it over-reports the current context; the context size is the LAST step-finish's
// input instead (see lastContextUsage).
type opencodeExport struct {
	Messages []struct {
		Info struct {
			Role string `json:"role"` // "user" | "assistant"
			Time struct {
				Created int64 `json:"created"` // unix milliseconds
			} `json:"time"`
		} `json:"info"`
		Parts []struct {
			Type      string `json:"type"` // "text" | "tool" | "step-start" | "step-finish"
			Text      string `json:"text"`
			Synthetic bool   `json:"synthetic"`
			Ignored   bool   `json:"ignored"`
			Tokens    *struct {
				Input     int `json:"input"`
				Output    int `json:"output"`
				Reasoning int `json:"reasoning"`
				Cache     struct {
					Read  int `json:"read"`
					Write int `json:"write"`
				} `json:"cache"`
			} `json:"tokens"` // present only on a step-finish part
		} `json:"parts"`
	} `json:"messages"`
}

// stepUsage maps a step-finish part's tokens onto our Usage, matching the live
// parser (parseOpencodeStream) so the reattach badge equals the in-turn one:
// reasoning folds into Output, cache read/write map through.
func stepUsage(t *struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}) Usage {
	return Usage{
		Input:      t.Input,
		Output:     t.Output + t.Reasoning,
		CacheRead:  t.Cache.Read,
		CacheWrite: t.Cache.Write,
	}
}

// export runs `opencode export <id>` and unmarshals it. A malformed id, a failed
// command (missing/deleted session), or unparseable output all yield (zero, ok)
// rather than an error, matching the "missing file → empty" convention of the
// file-based readers.
func (fs opencodeFS) export(id string) (opencodeExport, bool) {
	var ex opencodeExport
	if !validOpencodeID(id) {
		return ex, false
	}
	out, err := fs.run("export", id)
	if err != nil {
		return ex, false
	}
	if json.Unmarshal(out, &ex) != nil {
		return ex, false
	}
	return ex, true
}

// exportMessages maps one exported session onto ordered conversation Messages.
// Each message's text parts join into its prose (synthetic/ignored skipped);
// tool-only / empty messages are dropped from the replay. A "claude" (assistant)
// message carries the usage of its last step-finish so the per-message context
// badge survives a reattach. Pure (no I/O) so it's directly testable.
func exportMessages(ex opencodeExport) []Message {
	var out []Message
	for _, m := range ex.Messages {
		var role string
		switch m.Info.Role {
		case "assistant":
			role = "claude"
		case "user":
			role = "user"
		default:
			continue
		}
		var text strings.Builder
		var usage *Usage
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				if p.Synthetic || p.Ignored || p.Text == "" {
					continue
				}
				if text.Len() > 0 {
					text.WriteString("\n\n")
				}
				text.WriteString(p.Text)
			case "step-finish":
				if p.Tokens != nil {
					u := stepUsage(p.Tokens)
					usage = &u // last step-finish in the message wins
				}
			}
		}
		t := strings.TrimSpace(text.String())
		if t == "" {
			continue // tool-only / empty turn: nothing to replay
		}
		msg := Message{Role: role, Text: t, Ts: m.Info.Time.Created / 1000}
		if role == "claude" && usage != nil && usage.Input+usage.CacheRead > 0 {
			msg.Usage = usage
		}
		out = append(out, msg)
	}
	return out
}

// exportContext returns a session's current context size: the last step-finish's
// tokens across all its messages (opencode reports the full prompt as that step's
// input). Unlike exportMessages this counts tool-only messages too — a turn that
// ended in a tool call still grew the context. nil if no usage-bearing step
// exists. Pure (no I/O).
func exportContext(ex opencodeExport) *ContextSnapshot {
	var last *Usage
	var at int64
	for _, m := range ex.Messages {
		for _, p := range m.Parts {
			if p.Type == "step-finish" && p.Tokens != nil {
				u := stepUsage(p.Tokens)
				last = &u
				at = m.Info.Time.Created / 1000
			}
		}
	}
	if last != nil && last.Input+last.CacheRead > 0 {
		return &ContextSnapshot{Usage: *last, At: at}
	}
	return nil
}

// readTranscriptChain concatenates the exported conversations for ids (oldest
// first) into one re-indexed history.
func (fs opencodeFS) readTranscriptChain(ids []string) ([]Message, error) {
	var all []Message
	for _, id := range ids {
		ex, ok := fs.export(id)
		if !ok {
			continue
		}
		all = append(all, exportMessages(ex)...)
	}
	for i := range all {
		all[i].Index = i
	}
	return all, nil
}

// lastContextUsage returns the newest session's context snapshot, scanning ids
// newest-first; nil if no id has a usage-bearing step yet.
func (fs opencodeFS) lastContextUsage(ids []string) *ContextSnapshot {
	for i := len(ids) - 1; i >= 0; i-- {
		ex, ok := fs.export(ids[i])
		if !ok {
			continue
		}
		if cx := exportContext(ex); cx != nil {
			return cx
		}
	}
	return nil
}

// deleteByIDs removes each opencode session via `opencode session delete`. It is
// best-effort: a delete that fails (e.g. the session is already gone) is skipped
// rather than aborting the batch, so deleting a partly-removed set still clears
// the rest. Returns the count actually removed.
func (fs opencodeFS) deleteByIDs(ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		if !validOpencodeID(id) {
			continue
		}
		if _, err := fs.run("session", "delete", id); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

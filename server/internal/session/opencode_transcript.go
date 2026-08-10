package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
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
func (fs opencodeFS) run(ctx context.Context, args ...string) ([]byte, error) {
	if fs.remote == nil {
		return exec.CommandContext(ctx, opencodeReaderBin, args...).Output()
	}
	return fs.remote.output(ctx, opencodeReaderBin+" "+strings.Join(args, " "))
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
			ID   string `json:"id"`   // opencode's durable msg_… id (stable across rotation)
			Role string `json:"role"` // "user" | "assistant"
			Time struct {
				Created int64 `json:"created"` // unix milliseconds
			} `json:"time"`
		} `json:"info"`
		Parts []opencodePart `json:"parts"`
	} `json:"messages"`
}

// opencodePart is one part of an exported message: prose, a tool call, or a step
// boundary. A step-finish carries that step's tokens and marks the end of one
// model request/response cycle (see exportMessages, which splits on it).
type opencodePart struct {
	Type      string          `json:"type"` // "text" | "tool" | "step-start" | "step-finish"
	Text      string          `json:"text"`
	Synthetic bool            `json:"synthetic"`
	Ignored   bool            `json:"ignored"`
	Tokens    *opencodeTokens `json:"tokens"` // present only on a step-finish part
}

// opencodeTokens is a step's token accounting as opencode reports it.
type opencodeTokens struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// stepUsage maps a step-finish part's tokens onto our Usage, matching the live
// parser (parseOpencodeStream) so the reattach badge equals the in-turn one:
// reasoning folds into Output, cache read/write map through.
func stepUsage(t *opencodeTokens) Usage {
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
func (fs opencodeFS) export(ctx context.Context, id string) (opencodeExport, bool) {
	var ex opencodeExport
	if !validOpencodeID(id) {
		return ex, false
	}
	out, err := fs.run(ctx, "export", id)
	if err != nil {
		return ex, false
	}
	if json.Unmarshal(out, &ex) != nil {
		return ex, false
	}
	return ex, true
}

// opencodeExportCache memoizes `opencode export <id>` output per session id
// (namespaced by host). The file-based backends get this for free — an unchanged
// file's parse is cached on its stat — but opencode has no per-session file to
// stat, so without a memo every chain read re-ran one export subprocess PER ID on
// the host, over SSH, even when the chain hadn't moved.
//
// Each entry records the store signature it was exported under. A hit needs
// either a matching signature or an immutable id (see exportCached).
type opencodeExportEntry struct {
	sig string // fs.storeSig at export time; "" when the store couldn't be signed
	ex  opencodeExport
}

// opencodeExportCacheCap bounds the memo: ids rotate on clear/compress, so retired
// ones would otherwise accumulate for the process's life. Far above any real
// working set, and dropped wholesale rather than by an LRU we'd have to maintain.
const opencodeExportCacheCap = 256

var (
	opencodeExportMu    sync.Mutex
	opencodeExportCache = map[string]opencodeExportEntry{}
)

// exportCached is export behind the memo. sig/sigOK are the store signature for
// this whole read, taken ONCE by the caller so a chain of N ids costs one stat
// rather than N.
//
// immutable says the id can no longer gain messages — it is a rotated-away id, not
// the chain's live one — and is what makes this pay off across turns: a stale
// store signature (any opencode session anywhere writing) invalidates the live id
// only, while the archived prefix of the chain, which is most of it, stays cached.
func (fs opencodeFS) exportCached(ctx context.Context, id, sig string, sigOK, immutable bool) (opencodeExport, bool) {
	key := fs.cacheKey(id)
	opencodeExportMu.Lock()
	e, hit := opencodeExportCache[key]
	opencodeExportMu.Unlock()
	if hit && (immutable || (sigOK && e.sig == sig)) {
		return e.ex, true
	}
	ex, ok := fs.export(ctx, id)
	if !ok {
		return ex, false
	}
	stored := ""
	if sigOK {
		stored = sig
	}
	opencodeExportMu.Lock()
	if len(opencodeExportCache) >= opencodeExportCacheCap {
		opencodeExportCache = map[string]opencodeExportEntry{}
	}
	opencodeExportCache[key] = opencodeExportEntry{sig: stored, ex: ex}
	opencodeExportMu.Unlock()
	return ex, true
}

// exportMessages maps one exported session onto ordered conversation Messages.
//
// An opencode assistant "message" is the whole agentic loop for one dictation:
// text, tool calls and several step-finish parts all live in the same record. A
// step is one model request/response cycle — Claude's `assistant` line — so the
// replay SPLITS an assistant message at its step boundaries, one Message per step
// that produced prose. Without that split a multi-step reply collapses into a
// single wall of text on reattach, unlike the live stream (which emits each text
// part as its own chunk) and unlike every other backend. Split segments after the
// first take the message id with a `#n` suffix so rows stay distinctly
// identifiable while the first keeps the durable opencode id.
//
// Within a segment the text parts join with a blank line (synthetic/ignored
// skipped); tool-only / empty segments are dropped. Usage lands on the LAST
// segment of a message (matching the live badge, which lands on the closing
// frame), as does the Turns/TurnTotal rollup for the dictation: how many steps ran
// since the last user message, and the final step's tokens — the same pair the
// live closing frame carries. Pure (no I/O) so it's directly testable.
func exportMessages(ex opencodeExport) []Message {
	var out []Message
	runTurns := 0             // step-finish parts since the last user message
	runTotal := (*Usage)(nil) // that run's last step tokens (== the live turn's Usage)
	lastClaude := -1
	flushRun := func() {
		if lastClaude >= 0 && runTurns > 0 {
			out[lastClaude].Turns, out[lastClaude].TurnTotal = runTurns, runTotal
		}
		runTurns, runTotal, lastClaude = 0, nil, -1
	}
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
		ts := m.Info.Time.Created / 1000
		if role == "user" {
			flushRun() // a new dictation begins; close out the previous run
			var text strings.Builder
			for _, p := range m.Parts {
				appendPartText(&text, p)
			}
			t := opencodeUnquote(strings.TrimSpace(text.String()))
			if t == "" {
				continue
			}
			out = append(out, Message{ID: m.Info.ID, Role: role, Text: t, Ts: ts})
			continue
		}
		// Assistant: emit one row per prose-bearing step.
		seg := 0
		var text strings.Builder
		var usage *Usage
		emit := func() {
			t := strings.TrimSpace(text.String())
			text.Reset()
			u := usage
			usage = nil
			if t == "" {
				return // tool-only step: nothing to replay
			}
			id := m.Info.ID
			if seg > 0 && id != "" {
				id = fmt.Sprintf("%s#%d", id, seg)
			}
			seg++
			msg := Message{ID: id, Role: role, Text: t, Ts: ts}
			if u != nil && u.Input+u.CacheRead > 0 {
				msg.Usage = u
			}
			out = append(out, msg)
			lastClaude = len(out) - 1
		}
		for _, p := range m.Parts {
			switch p.Type {
			case "text":
				appendPartText(&text, p)
			case "step-finish":
				runTurns++
				if p.Tokens != nil {
					u := stepUsage(p.Tokens)
					usage, runTotal = &u, &u
				}
				emit()
			}
		}
		emit() // trailing prose after the final step-finish
		// Only the message's closing row keeps a usage badge, matching the live
		// stream (chunks carry none; the closing frame does).
		for i := lastClaude - 1; i >= 0 && out[i].Role == "claude"; i-- {
			out[i].Usage = nil
		}
	}
	flushRun()
	return out
}

// appendPartText adds one text part's prose to a segment builder, skipping
// synthetic/ignored/empty parts and separating parts with a blank line.
func appendPartText(text *strings.Builder, p opencodePart) {
	if p.Type != "text" || p.Synthetic || p.Ignored || p.Text == "" {
		return
	}
	if text.Len() > 0 {
		text.WriteString("\n\n")
	}
	text.WriteString(p.Text)
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
func (fs opencodeFS) readTranscriptChain(ctx context.Context, ids []string) ([]Message, error) {
	sig, sigOK := fs.storeSig(ctx) // once for the whole chain, not once per id
	var all []Message
	for i, id := range ids {
		// Every id but the last has been rotated away and can no longer grow, so its
		// export stays valid however much the store moves underneath it.
		ex, ok := fs.exportCached(ctx, id, sig, sigOK, i < len(ids)-1)
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
func (fs opencodeFS) lastContextUsage(ctx context.Context, ids []string) *ContextSnapshot {
	sig, sigOK := fs.storeSig(ctx)
	for i := len(ids) - 1; i >= 0; i-- {
		ex, ok := fs.exportCached(ctx, ids[i], sig, sigOK, i < len(ids)-1)
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
func (fs opencodeFS) deleteByIDs(ctx context.Context, ids []string) (int, error) {
	n := 0
	for _, id := range ids {
		if !validOpencodeID(id) {
			continue
		}
		if _, err := fs.run(ctx, "session", "delete", id); err != nil {
			continue
		}
		n++
	}
	return n, nil
}

// opencodeStorePaths are the SQLite files opencode keeps its sessions in,
// relative to the host user's home. The -wal companion matters as much as the
// database itself: a just-written turn can sit entirely in the write-ahead log
// while opencode.db's own size and mtime are untouched, so signing only the
// database would report "unchanged" for a session that just grew.
var opencodeStorePaths = []string{
	".local/share/opencode/opencode.db",
	".local/share/opencode/opencode.db-wal",
}

// chainSig signs the opencode store rather than the chain. There is no per-session
// file to stat — history comes from `opencode export`, which is the expensive call
// the signature exists to avoid — but every session lives in one SQLite database,
// and any write to any session bumps that database's size/mtime (or its -wal's).
// So stat'ing the store and folding in the ids gives the contract a valid, cheap
// answer: an unchanged signature means no session changed, hence this chain didn't.
// It is deliberately conservative in the other direction — an unrelated session's
// turn changes the signature and costs one recompute — which is the cheap side of
// the trade against re-exporting the whole chain on every digest.
//
// The signature folds in SIZE ONLY, deliberately NOT mtime. Opening the SQLite
// store touches its mtime, and `opencode export` — the very call the signature
// exists to avoid — opens the store. Signing mtime therefore made every read
// invalidate its own cache entry: the 5-second auto-compress scan re-exported
// every opencode session forever, spawning a steady stream of `opencode`
// processes on the host that no user action explained. Size is the write-derived
// signal (turns append to the database or its -wal); mtime, here, only describes
// that somebody read.
//
// ok=false when neither store file can be stat'd (no opencode data yet, or an
// unreadable host), so the caller falls back to recomputing instead of trusting a
// signature that describes nothing.
func (fs opencodeFS) chainSig(ctx context.Context, ids []string) (string, bool) {
	sig, ok := fs.storeSig(ctx)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("%sids:%s", sig, strings.Join(ids, ",")), true
}

// storeSig is the id-independent half of chainSig: the signature of the opencode
// store itself. Split out because the export memo keys on it too — it is the one
// cheap "has any opencode session changed?" signal, and both callers should pay
// for it once per read rather than once per id.
func (fs opencodeFS) storeSig(ctx context.Context) (string, bool) {
	home, ok := fs.home(ctx)
	if !ok {
		return "", false
	}
	var b strings.Builder
	any := false
	for _, rel := range opencodeStorePaths {
		size, _, ok := fs.stat(ctx, path.Join(home, rel))
		if !ok {
			continue // absent (no -wal in the common case) — sign what exists
		}
		any = true
		fmt.Fprintf(&b, "%s:%d;", rel, size)
	}
	if !any {
		return "", false
	}
	return b.String(), true
}

// home resolves the session host's home directory: locally the server process's
// own, remotely the target's (the reader holds no config, and the remote user's
// home need not match the server's). Resolved once per chainSig so signing both
// store files costs one extra round trip, not one per file.
func (fs opencodeFS) home(ctx context.Context) (string, bool) {
	if fs.remote == nil {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return home, true
	}
	out, err := fs.remote.output(ctx, `printf %s "$HOME"`)
	if err != nil {
		return "", false
	}
	home := strings.TrimSpace(string(out))
	if home == "" {
		return "", false
	}
	return home, true
}

// opencodeUnquote undoes the double-quote wrapping opencode puts around a user
// message it received as a CLI argument: `opencode run -- <prompt>` stores the
// prompt as `"<prompt>"`, quotes included, and `opencode export` hands it back
// that way. Those two bytes are not the user's words, and leaving them on breaks
// two things downstream that both match on exact text: the gateway's
// stripInjected no longer recognizes its own trailing scaffolding (so the
// "(Reply briefly…)" hint leaks into the replayed bubble), and the app can no
// longer match a replayed row against the live row it already shows (so the
// message appears twice). Stripping it here — at the reader, where the backend's
// storage quirk is — means every consumer sees the text the user actually sent.
//
// Only one matching leading AND trailing quote is removed. A message the user
// really did wrap in quotes therefore loses them in replay — two cosmetic
// characters, against a bubble that otherwise duplicates and leaks scaffolding on
// every opencode turn.
func opencodeUnquote(t string) string {
	if len(t) < 2 || t[0] != '"' || t[len(t)-1] != '"' {
		return t
	}
	return strings.TrimSpace(t[1 : len(t)-1])
}

package gateway

import (
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/usage"
)

// Wire types for the WebSocket protocol (see docs/protocol.md). Inbound JSON is
// decoded into `inbound`; outbound messages are plain maps built by the helpers
// below so each carries only the fields it needs.

const serverVersion = "0.1.0"

// inbound is the union of fields any app->server message may carry.
type inbound struct {
	Type         string            `json:"type"`
	Token        string            `json:"token"`
	Text         string            `json:"text"`          // utterance / dialog reply text
	Name         string            `json:"name"`          // session name for attach/kill/rename
	NewName      string            `json:"new_name"`      // target name for rename
	Path         string            `json:"path"`          // directory for browse / spawn_at
	Target       string            `json:"target"`        // on spawn_at: "host" (default) | "sandbox" execution target
	Codec        string            `json:"codec"`         // audio codec on wake: "ogg_opus" | "pcm16"
	ClientID     string            `json:"client_id"`     // stable per-app id, for reconnect/resume
	HandsFree    bool              `json:"hands_free"`    // set on `wake` when the clip is VAD-gated (hands-free)
	EndToken     string            `json:"end_token"`     // on `hello`: the spoken word that commits a message
	SttMode      string            `json:"stt_mode"`      // on `hello`: "dynamic" | "fixed"
	SttModel     string            `json:"stt_model"`     // on `hello`: fixed model "tiny" | "base" | "small"
	Calibrate    bool              `json:"calibrate"`     // on `wake`: transcribe (fast model) and return, don't dispatch
	Aliases      map[string]string `json:"aliases"`       // on `hello`: mis-transcription -> canonical command word
	WhisperURL   string            `json:"whisper_url"`   // on `hello`: resident whisper server URL (overrides the default)
	WhisperModel string            `json:"whisper_model"` // on `hello`: ggml model to hot-load on the resident server (e.g. "medium.en")
	Before       *int              `json:"before"`        // on `history`: page cursor (exclusive index); nil = most recent
	Limit        int               `json:"limit"`         // on `history`: page size (default 30)
	Silent       bool              `json:"silent"`        // on `attach`: suppress the spoken "attached…" confirmation (reconnect auto-attach)
	SessionID    string            `json:"session_id"`    // on `adopt`: the discovered Claude session_id to register
	Brief        bool              `json:"brief"`         // on `hello`: append a "reply briefly for TTS" hint to dictation
	Interactive  bool              `json:"interactive"`   // on `hello`: let Claude ask clarifying questions mid-task
}

func msgHelloOK(sessionID, whisperModel string) map[string]any {
	return map[string]any{"type": "hello_ok", "server_version": serverVersion, "session_id": sessionID, "whisper_model": whisperModel}
}

// msgWhisperModel reports the resident whisper server's current model (server-
// global). Sent in hello_ok and broadcast to all clients when it changes.
func msgWhisperModel(name string) map[string]any {
	return map[string]any{"type": "whisper_model", "model": name}
}

func msgSay(text string) map[string]any {
	return map[string]any{"type": "say", "text": text}
}

// msgPending shows the live hands-free buffer as a draft (uncommitted) so the
// user sees what's captured before speaking the end token. Empty text clears it.
func msgPending(text string) map[string]any {
	return map[string]any{"type": "pending", "text": text}
}

// msgCalibration returns what the fast (detection) model heard for a calibration
// sample, so the app can measure end-token recognition reliability.
func msgCalibration(text string) map[string]any {
	return map[string]any{"type": "calibration", "text": text}
}

// msgActivity is a live "what Claude is doing now" indicator (thinking / running
// a tool / editing a file). Not spoken; replaced by the reply when it arrives.
func msgActivity(text string) map[string]any {
	return map[string]any{"type": "activity", "text": text}
}

// msgFiles lists the files Claude changed during the turn (basenames).
func msgFiles(files []string) map[string]any {
	return map[string]any{"type": "files", "files": files}
}

func msgTranscript(text string, final bool) map[string]any {
	return map[string]any{"type": "transcript", "text": text, "final": final}
}

func msgDialog(state, prompt string) map[string]any {
	return map[string]any{"type": "dialog", "state": state, "prompt": prompt}
}

// msgAttached confirms the attach. When the session already has an on-disk
// transcript, it carries the last turn's `usage` (the current context size) and
// `usage_at` (that turn's unix time) so the app shows the context meter — and the
// cache-warm state — immediately, without waiting for a live turn to complete.
func msgAttached(name string, cx *session.ContextSnapshot) map[string]any {
	m := map[string]any{"type": "attached", "name": name}
	if cx != nil {
		m["usage"] = cx.Usage
		m["usage_at"] = cx.At
	}
	return m
}

func msgDetached() map[string]any {
	return map[string]any{"type": "detached"}
}

// msgContextReset tells the app the session's Claude context was rotated to a
// fresh one — a `clear` (empty) or a `compress` (seeded with a summary). The app
// drops its last-turn token accounting so the status-bar context-size readout
// returns to zero; no dictation has run against the new context yet, so there is
// nothing to show until the next turn lands (which reports the true new size).
func msgContextReset(name string) map[string]any {
	return map[string]any{"type": "context_reset", "name": name}
}

// msgRenamed tells the app that the currently-attached session was renamed
// (from the sidebar or the `rename` voice command). It carries the old and new
// names so the app can update the attached-session title in place, without the
// heavy re-attach side effects (history refetch, context-meter reseed) that a
// fresh `attached` message would trigger. Only sent when the rename follows the
// connection's attached session.
func msgRenamed(old, name string) map[string]any {
	return map[string]any{"type": "renamed", "old": old, "name": name}
}

// msgOutput carries session output for display + TTS. Live prose streams as
// chunk=true messages; the final chunk=false message closes the turn and (only
// then) carries the turn's token `usage`, which the app renders as a per-message
// badge. usage is nil for streaming chunks (no per-chunk accounting exists).
func msgOutput(name, text string, chunk bool, usage *session.Usage) map[string]any {
	m := map[string]any{"type": "output", "name": name, "text": text, "chunk": chunk}
	if usage != nil {
		m["usage"] = usage
	}
	return m
}

// msgRateLimit reports the Claude subscription's usage-window state (from the
// stream-json rate_limit_event) so the app can show the plan's session limit —
// which window is binding, when it resets, and a coarse status. Not spoken.
func msgRateLimit(rl session.RateLimit) map[string]any {
	return map[string]any{
		"type":          "rate_limit",
		"status":        rl.Status,
		"resets_at":     rl.ResetsAt,
		"limit_type":    rl.Type,
		"using_overage": rl.UsingOverage,
	}
}

// msgUsage carries the Claude plan's usage report (from `/usage`): the parsed
// session/weekly percent-used headline (pct = -1 when it couldn't be parsed) with
// reset times, plus the full report text for the app to show verbatim. Response
// to a `usage` request or the "usage" voice command; not spoken (the command path
// sends a separate `say` summary).
func msgUsage(sessionPct int, sessionReset string, weekPct int, weekReset, text string) map[string]any {
	return map[string]any{
		"type": "usage", "session_pct": sessionPct, "session_reset": sessionReset,
		"week_pct": weekPct, "week_reset": weekReset, "text": text,
	}
}

// msgUsageEstimate carries the server-global drift-live usage estimate (all
// sessions/clients). The *_est_pct fields drift up each turn; the *_real_pct
// fields are the last /usage calibration's true numbers; -1 means "not known
// yet" (uncalibrated). Sent after each turn and on /usage; also pushed on connect.
func msgUsageEstimate(v usage.View) map[string]any {
	return map[string]any{
		"type":               "usage_estimate",
		"calibrated":         v.Calibrated,
		"session_est_pct":    v.SessionEstPct,
		"week_est_pct":       v.WeekEstPct,
		"session_real_pct":   v.SessionRealPct,
		"week_real_pct":      v.WeekRealPct,
		"cum_tokens":         v.CumTokens,
		"tokens_since_check": v.TokensSinceCheck,
		"turns_since_check":  v.TurnsSinceCheck,
		"last_check_at":      v.LastCheckAt,
		"bench_set":          v.BenchSet,
		"bench_sess_pct":     v.BenchSessPct,
		"bench_week_pct":     v.BenchWeekPct,
		"bench_tokens":       v.BenchTokens,
		"tokens_since_set":   v.TokensSinceSet,
	}
}

func msgError(code, message string) map[string]any {
	return map[string]any{"type": "error", "code": code, "message": message}
}

// spokenError maps a protocol error code to a friendly, TTS-ready phrase spoken
// alongside the machine-readable `error` message, so a voice user hears why a
// command failed instead of silence. Only codes a voice user can actually
// trigger get an entry; wire-level / programmer-facing codes (bad_message,
// bad_adopt, bad_delete, bad_rename, unauthorized, internal) are intentionally
// absent — they come from the app, not from speech, and stay screen-only.
var spokenError = map[string]string{
	"bad_path":          "that path won't work, bud — it's outside where I can spawn, or it isn't a directory.",
	"not_found":         "couldn't find that, bud.",
	"no_session":        "there's no session by that name, bud.",
	"session_active":    "that session's open in a terminal — close it there first, bud.",
	"spawn_failed":      "couldn't start that session, bud.",
	"rename_failed":     "couldn't rename it, bud — that name might already be taken.",
	"usage_failed":      "couldn't check your usage right now, bud.",
	"compress_failed":   "the compress didn't go through, bud.",
	"turn_failed":       "that turn failed, bud.",
	"transcribe_failed": "I didn't catch that — the transcription failed.",
	"whisper_failed":    "the speech engine had a problem, bud.",
	"discover_failed":   "couldn't scan for sessions, bud.",
	"history_failed":    "couldn't load that session's history, bud.",
	"not_implemented":   "voice isn't set up on the server, bud — send text instead.",
}

func msgPong() map[string]any { return map[string]any{"type": "pong"} }

// msgTurnInterrupted tells the app that an in-flight dictation turn was abandoned
// server-side (the server is shutting down / restarting), so the app can clear
// its "thinking…" state and prompt the user to resend instead of waiting forever.
func msgTurnInterrupted(name, reason string) map[string]any {
	return map[string]any{"type": "turn_interrupted", "name": name, "reason": reason}
}

// msgAsk forwards Claude's mid-task clarification questions (interactive mode) to
// the app, which renders them (chips / text fields) and reads them aloud.
func msgAsk(name string, qs []askQuestion) map[string]any {
	return map[string]any{"type": "ask", "name": name, "questions": qs}
}

// msgDiff carries a compact `git diff --stat` review summary after a turn that
// changed files; the app shows it as a note (not spoken).
func msgDiff(text string) map[string]any {
	return map[string]any{"type": "diff", "text": text}
}

// msgTurnStopped tells the app a running turn was deliberately aborted (via the
// "abort" command / stop-turn button), so it clears the "thinking…" state
// without the "say it again" nudge of an interruption.
func msgTurnStopped(name string) map[string]any {
	return map[string]any{"type": "turn_stopped", "name": name}
}

// msgStopSpeaking tells the app to stop any in-progress text-to-speech (barge-in).
func msgStopSpeaking() map[string]any { return map[string]any{"type": "stop_speaking"} }

// msgHistory returns a page of a session's past conversation (older-to-newer),
// with `more` telling the app whether even-older messages remain to page in.
func msgHistory(name string, messages []session.Message, more bool) map[string]any {
	if messages == nil {
		messages = []session.Message{}
	}
	return map[string]any{"type": "history", "name": name, "messages": messages, "more": more}
}

// discoveredView is a Claude session found on disk (see session.Discovered),
// annotated with whether it's already in the registry and whether it looks live
// in tmux (adopting + driving it then risks a two-writer conflict).
type discoveredView struct {
	Name       string `json:"name"`
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id"`
	LastActive int64  `json:"last_active"` // unix seconds
	Active     bool   `json:"active"`      // interactive claude open in tmux at this dir
	Registered bool   `json:"registered"`  // already in the spawner registry
	Busy       bool   `json:"busy"`        // a dictation turn is running for this session now
	Target     string `json:"target,omitempty"` // execution target ("sandbox") when not the default host
}

func msgDiscovered(items []discoveredView) map[string]any {
	if items == nil {
		items = []discoveredView{}
	}
	return map[string]any{"type": "discovered", "sessions": items}
}

// msgReadLast tells the app to re-read (TTS) and scroll to the last `count`
// Claude replies in the current session's log.
func msgReadLast(count int) map[string]any {
	if count < 1 {
		count = 1
	}
	return map[string]any{"type": "read_last", "count": count}
}

func msgSessionList(sessions []sessionView) map[string]any {
	return map[string]any{"type": "session_list", "sessions": sessions}
}

type sessionView struct {
	Name string `json:"name"`
	Dir  string `json:"dir"`
	// Target is the execution target ("sandbox") when it isn't the default host, so
	// the app can badge sandbox sessions. Omitted (empty) for host sessions.
	Target string `json:"target,omitempty"`
}

// listingEntry is one directory in a browse listing.
type listingEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Repo bool   `json:"repo"` // true if it's a git repo
}

// msgListing is the response to a `browse`: the directory's subfolders, plus the
// parent path for "up" ("" means the parent is the roots view).
func msgListing(path, parent string, entries []listingEntry) map[string]any {
	return map[string]any{"type": "listing", "path": path, "parent": parent, "entries": entries}
}

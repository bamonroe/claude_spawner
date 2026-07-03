package gateway

import "github.com/bam/claude_spawner/server/internal/session"

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
}

func msgHelloOK(sessionID string) map[string]any {
	return map[string]any{"type": "hello_ok", "server_version": serverVersion, "session_id": sessionID}
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

func msgAttached(name string) map[string]any {
	return map[string]any{"type": "attached", "name": name}
}

func msgDetached() map[string]any {
	return map[string]any{"type": "detached"}
}

func msgOutput(name, text string, chunk bool) map[string]any {
	return map[string]any{"type": "output", "name": name, "text": text, "chunk": chunk}
}

func msgError(code, message string) map[string]any {
	return map[string]any{"type": "error", "code": code, "message": message}
}

func msgPong() map[string]any { return map[string]any{"type": "pong"} }

// msgTurnInterrupted tells the app that an in-flight dictation turn was abandoned
// server-side (the server is shutting down / restarting), so the app can clear
// its "thinking…" state and prompt the user to resend instead of waiting forever.
func msgTurnInterrupted(name, reason string) map[string]any {
	return map[string]any{"type": "turn_interrupted", "name": name, "reason": reason}
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

package gateway

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/spoken"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// clientState is what we stash when a connection drops, to resume on reconnect:
// an in-progress dialog. (Re-attaching to a session is client-driven — the app
// persists the session name and re-sends `attach`, which also survives a server
// restart because sessions are durable on disk.)
type clientState struct {
	dlg *dialog
}

// New builds a gateway Server. stt may be nil, in which case audio frames are
// rejected but text `utterance` messages still work. ttsClient may be nil, in
// which case `speak` requests are refused and clients use on-device TTS.

// conn is the per-connection state and read loop.
//
// Concurrency model: every field below is owned by the connection's READ LOOP
// — inbound messages are dispatched serially from loop(), so handlers never
// race each other — with three deliberate exceptions:
//
//   - wmu serializes websocket WRITES, because job goroutines (startTurn fan-out)
//     and the speak worker write concurrently with the read loop. closed rides
//     under wmu too: it's set after the read loop exits and read by job sinks.
//   - attachedMu guards `attached` for the rare cross-goroutine READER
//     (Server.NotifyShutdown). The read loop remains the only writer, via
//     setAttached; loop-side reads of c.attached need no lock.
//   - speakMu guards speakCancel (read loop's speak_stop vs the speak worker).
//
// Turn goroutines never touch conn state directly: startTurn captures the
// *session.Session and talks back through the locked sessionJob hub (see
// jobs.go for its lock-ordering note: j.mu -> conn.wmu, never the reverse).
type conn struct {
	srv      *Server
	ws       *websocket.Conn
	ctx      context.Context
	clientID string // stable per-app id from hello, for resume

	wmu    sync.Mutex // guards writes (job goroutines also write) AND closed
	closed bool       // set once the connection is gone (guards job delivery)

	attachedMu    sync.Mutex       // guards attached for cross-goroutine readers; see setAttached
	attached      *session.Session // non-nil when in passthrough mode
	prevSessionID string           // session_id of the session attached just before this one — the "swap" target (survives renames)
	dlg           *dialog          // non-nil while a dialog is in progress

	collecting     bool   // between `wake` and `audio_end`
	audio          []byte // accumulated audio for the current utterance
	audioCodec     string // "ogg_opus" (compressed) or "pcm16" (raw)
	audioSessionID string // app-declared target session for the current audio clip
	gated          bool   // current utterance is hands-free (VAD-gated → accumulate)
	calibrate      bool   // current utterance is an end-token calibration sample

	buffer          []string          // hands-free rough draft (per-chunk fast transcripts, for detection)
	audioPCM        []byte            // hands-free raw PCM of all chunks, re-transcribed as one on commit
	bufferSessionID string            // app-declared target session for the hands-free buffer
	brief           bool              // append a "reply briefly for TTS" hint to dictation
	interactive     bool              // let Claude ask clarifying questions mid-task
	dictationGate   bool              // discard un-bracketed speech instead of dictating it (needs a speech-gate token configured)
	sttMode         string            // "dynamic" | "fixed" whisper model selection
	sttModel        string            // fixed-mode model: "tiny" | "base" | "small"
	wakeService     string            // live wake/end-token backend: "whisper" (default string-match) | "detector" (the SPAWNER_WAKEWORD_URL sidecar)
	aliases         map[string]string // mis-transcription -> canonical command word
	scratch         bool              // scratch mode: while detached, echo each transcription back aloud (STT test)

	speakCh     chan speakReq      // queued `speak` requests, drained in order by speakWorker; nil = server TTS disabled
	speakMu     sync.Mutex         // guards speakCancel (read loop vs speak worker)
	speakCancel context.CancelFunc // aborts the in-flight synthesis (speak_stop); nil = none running
}

// setAttached records which session this connection is attached to (nil =
// detached). Read-loop only — it is the single writer; the lock exists so the
// cross-goroutine reader (attachedSession) sees a consistent value.

// setAttached records which session this connection is attached to (nil =
// detached). Read-loop only — it is the single writer; the lock exists so the
// cross-goroutine reader (attachedSession) sees a consistent value.
func (c *conn) setAttached(s *session.Session) {
	c.attachedMu.Lock()
	c.attached = s
	c.attachedMu.Unlock()
}

// attachedSession is the cross-goroutine-safe reader for c.attached, for the
// few places outside the read loop that need it (Server.NotifyShutdown). Code
// running IN the read loop just reads c.attached directly.

// attachedSession is the cross-goroutine-safe reader for c.attached, for the
// few places outside the read loop that need it (Server.NotifyShutdown). Code
// running IN the read loop just reads c.attached directly.
func (c *conn) attachedSession() *session.Session {
	c.attachedMu.Lock()
	defer c.attachedMu.Unlock()
	return c.attached
}

// transcriber returns this connection's STT — the server default (the whisper
// server is fixed by config; clients no longer override the URL).

// transcriber returns this connection's STT — the server default (the whisper
// server is fixed by config; clients no longer override the URL).
func (c *conn) transcriber() transcribe.Transcriber {
	return c.srv.stt
}

// fastTranscriber returns the fast draft/detection STT (a small model on its own
// server), falling back to the main one if none is configured.

// fastTranscriber returns the fast draft/detection STT (a small model on its own
// server), falling back to the main one if none is configured.
func (c *conn) fastTranscriber() transcribe.Transcriber {
	if c.srv.fastStt != nil {
		return c.srv.fastStt
	}
	return c.transcriber()
}

// send writes a JSON message to the client, returning any write error (also used
// by job sinks to tell a delivered result from one lost to a dropped socket).

// send writes a JSON message to the client, returning any write error (also used
// by job sinks to tell a delivered result from one lost to a dropped socket).
func (c *conn) send(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteJSON(v); err != nil {
		log.Printf("ws write: %v", err)
		return err
	}
	return nil
}

// sendBinary writes one binary frame (server→client TTS audio) under the same
// write lock as JSON messages, so audio frames and control traffic never
// interleave mid-write.

// sendBinary writes one binary frame (server→client TTS audio) under the same
// write lock as JSON messages, so audio frames and control traffic never
// interleave mid-write.
func (c *conn) sendBinary(data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(writeWait))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, data); err != nil {
		log.Printf("ws write: %v", err)
		return err
	}
	return nil
}

// fail sends the machine-readable `error` message to the client and, when the
// code has a friendly phrasing in spokenError, also speaks it — so a voice user
// isn't left with a silent failure. Use this in place of send(msgError(...)) at
// any site a spoken command can reach.

// fail sends the machine-readable `error` message to the client and, when the
// code has a friendly phrasing in spokenError, also speaks it — so a voice user
// isn't left with a silent failure. Use this in place of send(msgError(...)) at
// any site a spoken command can reach.
func (c *conn) fail(code, message string) {
	c.send(msgError(code, message))
	if spoken := spokenError[code]; spoken != "" {
		c.send(msgSay(spoken))
	}
}

// authenticate requires the first message to be a valid hello.

// restoreState re-applies any saved attach/dialog state for this client, so a
// reconnect resumes seamlessly. Runs after the hello_ok is sent.
func (c *conn) restoreState() {
	if c.clientID == "" {
		return
	}
	c.srv.clientsMu.Lock()
	st := c.srv.clients[c.clientID]
	delete(c.srv.clients, c.clientID)
	c.srv.clientsMu.Unlock()
	if st == nil || st.dlg == nil {
		return
	}
	// Resume an in-progress dialog where it left off.
	c.dlg = st.dlg
	c.send(msgSay("picking up where we left off."))
	c.repromptDialog()
}

// saveState stashes the current attach/dialog state for a future reconnect
// (in-memory; a server restart drops dialogs, but durable sessions let the app
// re-attach on its own).

// saveState stashes the current attach/dialog state for a future reconnect
// (in-memory; a server restart drops dialogs, but durable sessions let the app
// re-attach on its own).
func (c *conn) saveState() {
	if c.clientID == "" {
		return
	}
	c.srv.clientsMu.Lock()
	defer c.srv.clientsMu.Unlock()
	if c.dlg == nil {
		delete(c.srv.clients, c.clientID)
		return
	}
	c.srv.clients[c.clientID] = &clientState{dlg: c.dlg}
}

// keepAlive pings the client every pingPeriod until stop is closed. A failed ping
// (dead socket) ends it; the read loop then tears the connection down when its
// read deadline lapses with no pong.

// keepAlive pings the client every pingPeriod until stop is closed. A failed ping
// (dead socket) ends it; the read loop then tears the connection down when its
// read deadline lapses with no pong.
func (c *conn) keepAlive(stop <-chan struct{}) {
	t := time.NewTicker(pingPeriod)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}

// wakePhrases / speakPhrases / endPhrases resolve the connection's live wake, speak
// (dictation-gate) and end token phrases from the app-managed spoken-token
// catalogue — reading the store on each call so a catalogue edit takes effect for
// every connection immediately, without a reconnect. These sets REPLACE the old
// hardcoded "hey buddy"/"beep" built-ins (which survive only as the store's seed).
func (c *conn) wakePhrases() [][]string {
	return spoken.Phrases(c.srv.tokens.List(), spoken.ActionWake)
}

func (c *conn) speakPhrases() [][]string {
	return spoken.Phrases(c.srv.tokens.List(), spoken.ActionSpeechGate)
}

func (c *conn) endPhrases() [][]string { return spoken.Phrases(c.srv.tokens.List(), spoken.ActionEnd) }

// endModels are the distinct detector (ONNX) model keys bound to end-token tokens,
// scored against the wakeword sidecar when the detector service is on.

// endModels are the distinct detector (ONNX) model keys bound to end-token tokens,
// scored against the wakeword sidecar when the detector service is on.
func (c *conn) endModels() []string { return spoken.Models(c.srv.tokens.List(), spoken.ActionEnd) }

// stripWake / splitWake are the connection-scoped wake matchers: they match the
// configured wake phrases (c.wakePhrases()). Use these instead of the package-level
// command.StripWake / command.SplitWake anywhere a *conn is in scope.

// stripWake / splitWake are the connection-scoped wake matchers: they match the
// configured wake phrases (c.wakePhrases()). Use these instead of the package-level
// command.StripWake / command.SplitWake anywhere a *conn is in scope.
func (c *conn) stripWake(text string) (rest string, hadWake bool) {
	return command.StripWakeWith(text, c.wakePhrases())
}

func (c *conn) splitWake(text string) (before, after string, found bool) {
	return command.SplitWakeWith(text, c.wakePhrases())
}

func (c *conn) splitWakeAll(text string) (before string, commands []string) {
	return command.SplitWakeAllWith(text, c.wakePhrases())
}

// gateDictation applies the dictation gate to a would-be dictation string. When
// the gate is on (and a speak token is configured), only the text following the
// speak token is dictated (the token stripped); text with no speak token returns
// "" so the caller drops it as ambient chatter. Gate off — or no speak token —
// passes text through unchanged, preserving the ungated behavior. Commands are
// never routed through here, so "hey buddy stop" always works regardless.

// gateDictation applies the dictation gate to a would-be dictation string. When
// the gate is on (and a speak token is configured), only the text following the
// speak token is dictated (the token stripped); text with no speak token returns
// "" so the caller drops it as ambient chatter. Gate off — or no speak token —
// passes text through unchanged, preserving the ungated behavior. Commands are
// never routed through here, so "hey buddy stop" always works regardless.
func (c *conn) gateDictation(text string) string {
	speak := c.speakPhrases()
	if !c.dictationGate || len(speak) == 0 {
		return text
	}
	if _, after, found := command.SplitOn(text, speak); found {
		return strings.TrimSpace(after)
	}
	return ""
}

// handleUtterance routes a transcribed utterance to the active dialog, to a
// control command, or to dictation. sessionID is the app-declared target for this
// utterance; when present it reconciles the server's connection attachment before
// command/dictation routing.

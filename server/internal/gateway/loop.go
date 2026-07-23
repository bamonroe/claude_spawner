package gateway

import (
	"encoding/json"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/command"
)

// loop reads and dispatches messages until the socket closes. Text frames are
// JSON control/utterance messages routed through wireHandlers; binary frames are
// PCM16 audio for the utterance currently being recorded (between `wake` and
// `audio_end`).
func (c *conn) loop() {
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return // client gone (or read deadline lapsed with no pong)
		}
		_ = c.ws.SetReadDeadline(time.Now().Add(pongWait)) // any frame proves liveness
		if mt == websocket.BinaryMessage {
			c.handleAudioFrame(data)
			continue
		}

		var in inbound
		if err := json.Unmarshal(data, &in); err != nil {
			c.fail("bad_message", "invalid json")
			continue
		}
		if h := wireHandlers[in.Type]; h != nil {
			h(c, in)
		} else {
			c.fail("bad_message", "unknown message type: "+in.Type)
		}
	}
}

// wakePhrases / speakPhrases / endPhrases resolve the connection's live wake, speak
// (dictation-gate) and end token phrases from the app-managed spoken-token
// catalogue — reading the store on each call so a catalogue edit takes effect for
// every connection immediately, without a reconnect. These sets REPLACE the old
// hardcoded "hey buddy"/"beep" built-ins (which survive only as the store's seed).

// handleUtterance routes a transcribed utterance to the active dialog, to a
// control command, or to dictation. sessionID is the app-declared target for this
// utterance; when present it reconciles the server's connection attachment before
// command/dictation routing.
func (c *conn) handleUtterance(text, sessionID string) {
	// "stop" (barge-in) is intercepted everywhere: it stops speech without
	// disturbing dialog state and is never dictated to Claude.
	rest, _ := c.stripWake(text)
	if command.Parse(rest).Kind == command.Stop {
		c.send(msgStopSpeaking())
		return
	}
	if c.dlg != nil {
		c.handleDialog(text)
		return
	}
	if !c.selectClientSession(sessionID) {
		return
	}
	c.dispatch(text)
}

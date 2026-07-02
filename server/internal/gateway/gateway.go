// Package gateway implements the WebSocket gateway: one authenticated connection
// per app, carrying control commands and dictation. It wires the command parser
// (internal/command), the spawn dialog FSM, and the headless session driver
// (internal/session) together. The audio path (wake/binary/audio_end ->
// server-side Whisper) is not yet implemented; for now the app sends already-
// transcribed text as `utterance`/`reply` messages. See docs/protocol.md.
package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// Server holds the shared dependencies for all connections.
type Server struct {
	cfg      *config.Config
	store    *session.Store
	driver   *session.Driver
	babysit  *tmux.Manager
	stt      transcribe.Transcriber // nil disables the audio path
	projects *projects.Index        // fuzzy directory lookup for the spawn dialog
	up       websocket.Upgrader

	clientsMu sync.Mutex
	clients   map[string]*clientState // per-app resume state, keyed by client_id

	jobsMu sync.Mutex
	jobs   map[string]*sessionJob // running/last dictation turn, keyed by session name
}

// clientState is what we stash when a connection drops, to resume on reconnect:
// an in-progress dialog. (Re-attaching to a session is client-driven — the app
// persists the session name and re-sends `attach`, which also survives a server
// restart because sessions are durable on disk.)
type clientState struct {
	dlg *dialog
}

// New builds a gateway Server. stt may be nil, in which case audio frames are
// rejected but text `utterance` messages still work.
func New(cfg *config.Config, store *session.Store, driver *session.Driver, babysit *tmux.Manager, stt transcribe.Transcriber, proj *projects.Index) *Server {
	return &Server{
		cfg:      cfg,
		store:    store,
		driver:   driver,
		babysit:  babysit,
		stt:      stt,
		projects: proj,
		clients:  map[string]*clientState{},
		jobs:     map[string]*sessionJob{},
		up: websocket.Upgrader{
			// The app authenticates with a token in the hello message, so origin
			// checks add little; the network boundary (Tailscale/proxy) is the gate.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
}

const handshakeTimeout = 10 * time.Second

// HandleWS upgrades the request and runs the connection until it closes.
func (s *Server) HandleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := s.up.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("ws upgrade: %v", err)
		return
	}
	defer ws.Close()

	c := &conn{srv: s, ws: ws}
	ctx, cancel := context.WithCancel(r.Context())
	c.ctx = ctx
	defer cancel()

	if !c.authenticate() {
		return
	}
	c.restoreState() // re-attach / resume dialog from a previous connection
	c.loop()

	// Connection gone: stop delivering job events to it (so an in-flight turn
	// buffers its result for the next reconnect instead of dropping it).
	c.closed = true
	if c.attached != nil {
		c.srv.unbindJob(c.attached.Name)
	}
	c.saveState() // stash state so the next reconnect can resume
}

// jobSink returns a sink for session-job events that reports whether it actually
// reached this (still-connected) client.
func (c *conn) jobSink() func(any) bool {
	return func(v any) bool {
		if c.closed {
			return false
		}
		c.send(v)
		return true
	}
}

// conn is the per-connection state and read loop.
type conn struct {
	srv      *Server
	ws       *websocket.Conn
	ctx      context.Context
	clientID string // stable per-app id from hello, for resume

	wmu    sync.Mutex // guards writes (job goroutines also write)
	closed bool       // set once the connection is gone (guards job delivery)

	attached *session.Session // non-nil when in passthrough mode
	dlg      *dialog          // non-nil while a dialog is in progress

	collecting bool   // between `wake` and `audio_end`
	audio      []byte // accumulated audio for the current utterance
	audioCodec string // "ogg_opus" (compressed) or "pcm16" (raw)
	gated      bool   // current utterance is hands-free (VAD-gated → accumulate)
	calibrate  bool   // current utterance is an end-token calibration sample

	buffer   []string               // hands-free rough draft (per-chunk fast transcripts, for detection)
	audioPCM []byte                 // hands-free raw PCM of all chunks, re-transcribed as one on commit
	endToken string                 // spoken word that commits the buffer (default "beep")
	sttMode  string                 // "dynamic" | "fixed" whisper model selection
	sttModel string                 // fixed-mode model: "tiny" | "base" | "small"
	aliases  map[string]string      // mis-transcription -> canonical command word
	stt      transcribe.Transcriber // per-conn override (app-set whisper URL); nil = server default
}

// transcriber returns this connection's STT — an app-set override if present,
// else the server default.
func (c *conn) transcriber() transcribe.Transcriber {
	if c.stt != nil {
		return c.stt
	}
	return c.srv.stt
}

func (c *conn) send(v any) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteJSON(v); err != nil {
		log.Printf("ws write: %v", err)
	}
}

// authenticate requires the first message to be a valid hello.
func (c *conn) authenticate() bool {
	_ = c.ws.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var in inbound
	if err := c.ws.ReadJSON(&in); err != nil {
		return false
	}
	_ = c.ws.SetReadDeadline(time.Time{}) // clear deadline
	if in.Type != "hello" || in.Token == "" || in.Token != c.srv.cfg.AuthToken {
		c.send(msgError("unauthorized", "bad or missing token"))
		return false
	}
	c.clientID = in.ClientID
	c.endToken = strings.TrimSpace(in.EndToken)
	if c.endToken == "" {
		c.endToken = "beep"
	}
	c.sttMode = in.SttMode
	c.sttModel = in.SttModel
	c.aliases = in.Aliases
	if u := strings.TrimSpace(in.WhisperURL); u != "" {
		c.stt = &transcribe.RemoteWhisper{URL: u} // app-chosen resident whisper server
	}
	c.send(msgHelloOK("ws"))
	return true
}

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
	c.send(msgSay("picking up where we left off, bud."))
	c.repromptDialog()
}

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

// loop reads and dispatches messages until the socket closes. Text frames are
// JSON control/utterance messages; binary frames are PCM16 audio for the
// utterance currently being recorded (between `wake` and `audio_end`).
func (c *conn) loop() {
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			return // client gone
		}
		if mt == websocket.BinaryMessage {
			c.handleAudioFrame(data)
			continue
		}

		var in inbound
		if err := json.Unmarshal(data, &in); err != nil {
			c.send(msgError("bad_message", "invalid json"))
			continue
		}
		switch in.Type {
		case "ping":
			c.send(msgPong())
		case "utterance", "reply":
			c.gated = false // typed/explicit text is never background-gated
			c.handleUtterance(in.Text)
		case "attach":
			c.doAttach(in.Name, in.Silent)
		case "detach":
			c.doDetach()
		case "list_sessions":
			c.sendSessionList()
		case "rename":
			c.doRename(in.Name, in.NewName)
		case "delete":
			c.doDelete(in.Name)
		case "browse":
			c.doBrowse(in.Path)
		case "spawn_at":
			c.doSpawnAt(in.Path)
		case "cancel":
			c.cancelDialog()
		case "wake":
			c.startAudio(in.Codec, in.HandsFree, in.Calibrate)
		case "commit":
			c.commitMessage() // silence-timeout commit of the hands-free buffer
		case "history":
			c.serveHistory(in.Name, in.Before, in.Limit)
		case "audio_end":
			c.endAudio()
		default:
			c.send(msgError("bad_message", "unknown message type: "+in.Type))
		}
	}
}

// handleUtterance routes a transcribed utterance to the active dialog, to a
// control command, or to dictation, depending on connection state.
func (c *conn) handleUtterance(text string) {
	// "stop" (barge-in) is intercepted everywhere: it stops speech without
	// disturbing dialog state and is never dictated to Claude.
	rest, _ := command.StripWake(text)
	if command.Parse(rest).Kind == command.Stop {
		c.send(msgStopSpeaking())
		return
	}
	if c.dlg != nil {
		c.handleDialog(text)
		return
	}
	c.dispatch(text)
}

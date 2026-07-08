// Package gateway implements the WebSocket gateway: one authenticated connection
// per app, carrying control commands, dictation, and audio. It wires the command
// parser (internal/command), the spawn dialog FSM, the headless session driver
// (internal/session), and server-side Whisper (internal/transcribe) together. The
// app can send already-transcribed text as `utterance`, or stream audio
// (wake/binary/audio_end) for the server to transcribe. See docs/protocol.md.
package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
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
	"github.com/bam/claude_spawner/server/internal/usage"
)

// Server holds the shared dependencies for all connections.
type Server struct {
	cfg      *config.Config
	store    *session.Store
	hosts    *session.HostStore     // app-managed SSH host registry (Settings → Hosts)
	ids      *session.IdentityStore // app-managed SSH identity registry (Settings → Identities)
	driver   *session.Driver
	tmuxMgr  *tmux.Manager
	stt      transcribe.Transcriber // nil disables the audio path
	fastStt  transcribe.Transcriber // fast model for live drafts/detection; nil → use stt
	projects *projects.Index        // fuzzy directory lookup for the spawn dialog
	up       websocket.Upgrader

	clientsMu sync.Mutex
	clients   map[string]*clientState // per-app resume state, keyed by client_id

	jobsMu sync.Mutex
	jobs   map[string]*sessionJob // running/last dictation turn, keyed by session_id (stable across rename)

	inflight      *inflightTracker // sessions with a turn running now (persisted), keyed by session_id
	interruptedMu sync.Mutex
	interrupted   map[string]bool // session_ids whose turn was cut off by the last restart

	connsMu sync.Mutex
	conns   map[*conn]bool // currently-connected apps, for shutdown broadcasts

	whisperMu     sync.Mutex // guards the resident whisper server's currently-loaded model
	whisperLoaded string     // "<url>|<model>" last hot-loaded, to skip redundant /load calls
	currentModel  string     // the resident server's model NAME (server-global; apps read it)

	rateLimitMu sync.Mutex        // guards the last-seen subscription rate-limit state
	rateLimit   session.RateLimit // account-global; cached from turns, pushed to apps on connect

	usage *usage.Estimator // server-global drift-live usage estimate (all sessions/clients)
}

// lastRateLimit returns the most recent subscription usage-window state (empty
// Type until a turn has reported one).
func (s *Server) lastRateLimit() session.RateLimit {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	return s.rateLimit
}

// setRateLimit caches the latest rate-limit state seen on a turn, so a freshly
// connected app can be shown the plan's session limit without dictating first.
func (s *Server) setRateLimit(rl session.RateLimit) {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	s.rateLimit = rl
}

// currentWhisperModel returns the resident server's model name (server-global
// state that apps read on connect).
func (s *Server) currentWhisperModel() string {
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	return s.currentModel
}

// setWhisperModel hot-loads `name` onto the resident whisper server (at the
// server's configured URL) and records it as the current model. Blocks on the
// /load; call it from a goroutine. name maps to /models/ggml-<name>.bin.
func (s *Server) setWhisperModel(name string) error {
	url := s.cfg.WhisperURL
	if url == "" {
		return fmt.Errorf("no resident whisper server configured")
	}
	if !validModelName(name) {
		return fmt.Errorf("invalid model name %q", name)
	}
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	key := url + "|" + name
	if s.whisperLoaded == key {
		s.currentModel = name
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := transcribe.LoadRemoteModel(ctx, url, "/models/ggml-"+name+".bin"); err != nil {
		return fmt.Errorf("load %s: %w", name, err)
	}
	s.whisperLoaded = key
	s.currentModel = name
	log.Printf("whisper: model -> %s", name)
	return nil
}

// broadcastWhisperModel tells every connected app the current resident model, so
// a change made by one client updates all of them.
func (s *Server) broadcastWhisperModel(name string) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(msgWhisperModel(name))
	}
}

// broadcastUsageEstimate pushes the current server-global usage estimate to every
// connected app (it aggregates all sessions/clients, so everyone sees the same
// number). Sent after each turn's drift and after a /usage calibration.
func (s *Server) broadcastUsageEstimate(v usage.View) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	msg := msgUsageEstimate(v)
	for _, c := range cs {
		c.send(msg)
	}
}

// validModelName guards the model path against injection (letters, digits, dot,
// dash, underscore only).
func validModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
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
func New(cfg *config.Config, store *session.Store, hosts *session.HostStore, ids *session.IdentityStore, driver *session.Driver, tmuxMgr *tmux.Manager, stt transcribe.Transcriber, proj *projects.Index) *Server {
	var fast transcribe.Transcriber
	if cfg.WhisperFastURL != "" {
		fast = &transcribe.RemoteWhisper{URL: cfg.WhisperFastURL}
	}
	inflightPath, usagePath := "", ""
	if cfg.StatePath != "" {
		inflightPath = filepath.Join(filepath.Dir(cfg.StatePath), "inflight.json")
		usagePath = filepath.Join(filepath.Dir(cfg.StatePath), "usage_estimate.json")
	}
	inflight, interrupted := newInflightTracker(inflightPath)
	s := &Server{
		cfg:          cfg,
		store:        store,
		hosts:        hosts,
		ids:          ids,
		driver:       driver,
		tmuxMgr:      tmuxMgr,
		stt:          stt,
		fastStt:      fast,
		projects:     proj,
		clients:      map[string]*clientState{},
		jobs:         map[string]*sessionJob{},
		conns:        map[*conn]bool{},
		inflight:     inflight,
		interrupted:  interrupted,
		usage:        usage.Open(usagePath),
		currentModel: cfg.WhisperModelName, // authoritative from boot; loaded below
		up: websocket.Upgrader{
			// The app authenticates with a token in the hello message, so origin
			// checks add little; the network boundary (Tailscale/proxy) is the gate.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	// Load the configured default onto the resident server so its model matches
	// what we report to apps. Async so a big model doesn't delay startup.
	if cfg.WhisperURL != "" && cfg.WhisperModelName != "" {
		go func() {
			if err := s.setWhisperModel(cfg.WhisperModelName); err != nil {
				log.Printf("whisper: startup load failed: %v", err)
			}
		}()
	}
	return s
}

const handshakeTimeout = 10 * time.Second

// writeWait bounds a single websocket write. Without it a write to a client that
// dropped off the network (no FIN/RST yet) could block indefinitely; with it the
// write fails, which lets a job buffer its result for delivery on reconnect.
const writeWait = 10 * time.Second

// Keepalive: the server pings each client every pingPeriod and requires a pong
// (or any other frame) within pongWait, so a client that drops off the network is
// detected and torn down in ~tens of seconds instead of only when a write to it
// fails. pongWait must comfortably exceed pingPeriod (and the app's own 20 s ping
// interval) so a briefly-slow client isn't dropped.
const (
	pongWait   = 30 * time.Second
	pingPeriod = 12 * time.Second
)

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
	s.register(c)
	defer s.unregister(c)
	c.restoreState() // re-attach / resume dialog from a previous connection

	// Keepalive: require traffic (pongs to our pings, or any frame) within pongWait
	// so a client that vanishes off the network is detected promptly.
	_ = c.ws.SetReadDeadline(time.Now().Add(pongWait))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongWait))
	})
	stopPing := make(chan struct{})
	go c.keepAlive(stopPing)

	c.loop()
	close(stopPing)

	// Connection gone: stop delivering job events to it (so an in-flight turn
	// buffers its result for the next reconnect instead of dropping it).
	c.closed = true
	if c.attached != nil {
		c.srv.unbindJob(c, c.attached.SessionID)
	}
	c.saveState() // stash state so the next reconnect can resume
}

func (s *Server) register(c *conn) {
	s.connsMu.Lock()
	s.conns[c] = true
	s.connsMu.Unlock()
}

func (s *Server) unregister(c *conn) {
	s.connsMu.Lock()
	delete(s.conns, c)
	s.connsMu.Unlock()
}

// NotifyShutdown tells every connected app that the server is going down, so any
// in-flight dictation turn (which dies with this process — turns aren't persisted
// across a restart) surfaces as an interruption instead of the app waiting on a
// reply that will never come. Best-effort: called just before the HTTP shutdown.
func (s *Server) NotifyShutdown() {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		if c.attached == nil {
			continue
		}
		if j := s.job(c.attached.Name); j != nil && j.isRunning() {
			c.send(msgTurnInterrupted(c.attached.Name, "server restarting"))
		}
	}
}

// broadcast sends a message to every currently-connected app (best-effort; a
// failed write to a dropped socket is ignored).
func (s *Server) broadcast(v any) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(v)
	}
}

// job returns the session job for a name, if any.
func (s *Server) job(name string) *sessionJob {
	s.jobsMu.Lock()
	defer s.jobsMu.Unlock()
	return s.jobs[name]
}

// jobSink returns a sink for session-job events that reports whether it actually
// reached this client — true only if the connection is open AND the write
// succeeded. A failed write (dropped socket) returns false so the job buffers the
// result for delivery on reconnect instead of treating it as delivered and lost.
func (c *conn) jobSink() func(any) bool {
	return func(v any) bool {
		if c.closed {
			return false
		}
		return c.send(v) == nil
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

	buffer      []string               // hands-free rough draft (per-chunk fast transcripts, for detection)
	audioPCM    []byte                 // hands-free raw PCM of all chunks, re-transcribed as one on commit
	brief       bool                   // append a "reply briefly for TTS" hint to dictation
	interactive bool                   // let Claude ask clarifying questions mid-task
	endToken    string                 // spoken word that commits the buffer (default "beep")
	sttMode     string                 // "dynamic" | "fixed" whisper model selection
	sttModel    string                 // fixed-mode model: "tiny" | "base" | "small"
	aliases     map[string]string      // mis-transcription -> canonical command word
	stt         transcribe.Transcriber // per-conn override (app-set whisper URL); nil = server default
}

// transcriber returns this connection's STT — an app-set override if present,
// else the server default.
func (c *conn) transcriber() transcribe.Transcriber {
	if c.stt != nil {
		return c.stt
	}
	return c.srv.stt
}

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
func (c *conn) authenticate() bool {
	_ = c.ws.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var in inbound
	if err := c.ws.ReadJSON(&in); err != nil {
		return false
	}
	_ = c.ws.SetReadDeadline(time.Time{}) // clear deadline
	if in.Type != "hello" || subtle.ConstantTimeCompare([]byte(in.Token), []byte(c.srv.cfg.AuthToken)) != 1 {
		c.fail("unauthorized", "bad or missing token")
		return false
	}
	c.clientID = in.ClientID
	c.brief = in.Brief
	c.interactive = in.Interactive
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
	// The whisper model is server-global: the app reads it here rather than pushing
	// its own (so two clients don't bounce it), and changes it via set_whisper_model.
	c.send(msgHelloOK("ws", c.srv.currentWhisperModel()))
	// Push the last-known plan session-limit so the app can show it immediately,
	// rather than staying blank until the first turn of this connection.
	if rl := c.srv.lastRateLimit(); rl.Type != "" {
		c.send(msgRateLimit(rl))
	}
	// Same idea for the drift-live usage estimate — show it immediately on connect.
	if v := c.srv.usage.View(); v.Calibrated {
		c.send(msgUsageEstimate(v))
	}
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
	c.send(msgSay("picking up where we left off."))
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

// wireHandlers is the single registration table for the app→server control
// protocol: each inbound message `type` maps to the handler that services it.
// Adding a message means adding one entry here (and documenting it in
// docs/protocol.md — the internal/docsync drift test parses these keys and fails
// the build if any is undocumented). The hello handshake (authenticate) and
// binary audio frames (handleAudioFrame) are handled outside this table, before
// a frame ever reaches dispatch.
var wireHandlers = map[string]func(c *conn, in inbound){
	"ping":              func(c *conn, in inbound) { c.send(msgPong()) },
	"utterance":         func(c *conn, in inbound) { c.gated = false; c.handleUtterance(in.Text) }, // typed/explicit text is never background-gated
	"reply":             func(c *conn, in inbound) { c.gated = false; c.handleUtterance(in.Text) },
	"attach":            func(c *conn, in inbound) { c.doAttachBy(in.SessionID, in.Name, in.Silent) },
	"detach":            func(c *conn, in inbound) { c.doDetach() },
	"list_sessions":     func(c *conn, in inbound) { c.sendSessionList() },
	"discover":          func(c *conn, in inbound) { c.doDiscover() },
	"adopt":             func(c *conn, in inbound) { c.doAdopt(in.SessionID, in.Path) },
	"delete_discovered": func(c *conn, in inbound) { c.doDeleteDiscovered(in.SessionID) },
	"rename_discovered": func(c *conn, in inbound) { c.doRenameDiscovered(in.SessionID, in.Path, in.NewName) },
	"rename":            func(c *conn, in inbound) { c.doRename(in.Name, in.NewName) },
	"delete":            func(c *conn, in inbound) { c.doDelete(in.Name) },
	"browse":            func(c *conn, in inbound) { c.doBrowse(in.Path) },
	"spawn_at":          func(c *conn, in inbound) { c.doSpawnAt(in.Path, session.Target(in.Target), in.Create, in.HostName) },
	"cancel":            func(c *conn, in inbound) { c.cancelDialog() },
	"abort":             func(c *conn, in inbound) { c.abortTurn() },
	"set_whisper_model": func(c *conn, in inbound) { c.doSetWhisperModel(in.WhisperModel) },
	"restart":           func(c *conn, in inbound) { c.doRestart() },
	"wake":              func(c *conn, in inbound) { c.startAudio(in.Codec, in.HandsFree, in.Calibrate) },
	"commit":            func(c *conn, in inbound) { c.commitMessage() }, // silence-timeout commit of the hands-free buffer
	"discard_draft":     func(c *conn, in inbound) { c.clearBuffer() },   // drop the uncommitted hands-free draft
	"history":           func(c *conn, in inbound) { c.serveHistory(in.Name, in.Before, in.Limit) },
	"clear":             func(c *conn, in inbound) { c.doClear() },
	"compress":          func(c *conn, in inbound) { c.doCompress() },
	"usage":             func(c *conn, in inbound) { c.doUsage(false, usageCalibrate) }, // tap: show the report, don't speak it
	"usage_set":         func(c *conn, in inbound) { c.doUsage(false, usageSetBench) },  // "set" button: arm the benchmark
	"usage_calc":        func(c *conn, in inbound) { c.doUsage(false, usageCalcBench) }, // "calc" button: derive the rate
	"audio_end":         func(c *conn, in inbound) { c.endAudio() },
	"hosts":             func(c *conn, in inbound) { c.sendHostList() },
	"host_put":          func(c *conn, in inbound) { c.doHostPut(in.Host) },
	"host_delete":       func(c *conn, in inbound) { c.doHostDelete(in.Name) },
	"identities":        func(c *conn, in inbound) { c.sendIdentityList() },
	"identity_create":   func(c *conn, in inbound) { c.doIdentityCreate(in.Name) },
	"identity_import":   func(c *conn, in inbound) { c.doIdentityImport(in.Name, in.KeyPath) },
	"identity_delete":   func(c *conn, in inbound) { c.doIdentityDelete(in.Name) },
}

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

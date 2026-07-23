// Package gateway implements the WebSocket gateway: one authenticated connection
// per app, carrying control commands, dictation, and audio. It wires the command
// parser (internal/command), the spawn dialog FSM, the headless session driver
// (internal/session), and server-side Whisper (internal/transcribe) together. The
// app can send already-transcribed text as `utterance`, or stream audio
// (wake/binary/audio_end) for the server to transcribe. See docs/protocol.md.
package gateway

import (
	"context"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/detect"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
	"github.com/bam/claude_spawner/server/internal/tts"
)

// Server holds the shared dependencies for all connections.
type Server struct {
	cfg     *config.Config
	store   *session.Store
	hosts   *session.HostStore        // app-managed SSH host registry (Settings → Hosts)
	ids     *session.IdentityStore    // app-managed SSH identity registry (Settings → Identities)
	tokens  *session.SpokenTokenStore // app-managed spoken-token catalogue (wake/end/speak phrases + models)
	ssh     *session.SSHPool          // pooled SSH connections; nil when SSH-native is disabled
	driver  *session.Driver
	tmuxMgr *tmux.Manager
	stt     transcribe.Transcriber // nil disables the audio path
	fastStt transcribe.Transcriber // fast model for live drafts/detection; nil → use stt
	tts     *tts.Client            // server-side Kokoro synthesis; nil = clients use on-device TTS

	// detector gates the end token (and wake) on a clip via the purpose-trained
	// sidecar; nil → fall back to the Whisper string-match. wakeThreshold is the
	// score at/above which a token counts as fired.
	detector      detect.Detector
	wakeThreshold float64
	up            websocket.Upgrader

	clientsMu sync.Mutex
	clients   map[string]*clientState // per-app resume state, keyed by client_id

	jobsMu sync.Mutex
	jobs   map[string]*sessionJob // running/last dictation turn, keyed by session_id (stable across rename)

	inflight      *inflightTracker // sessions with a turn running now (persisted), keyed by session_id
	interruptedMu sync.Mutex
	interrupted   map[string]bool // session_ids whose turn was cut off by the last restart

	connsMu sync.Mutex
	conns   map[*conn]bool // currently-connected apps, for shutdown broadcasts

	modelMu        sync.Mutex // guards modelRefreshed (live backend-catalogue discovery throttle)
	modelRefreshed time.Time  // last model-discovery refresh; throttles the per-connect probe

	whisperMu     sync.Mutex // guards the resident whisper servers' currently-loaded models
	whisperLoaded string     // "<url>|<model>" last hot-loaded (accurate server), to skip redundant /load calls
	currentModel  string     // the accurate server's model NAME (server-global; apps read it)
	fastLoaded    string     // "<url>|<model>" last hot-loaded on the fast (draft/detection) server
	currentFast   string     // the fast server's model NAME ("" when no fast server is configured)

	downloadMu  sync.Mutex      // guards downloading
	downloading map[string]bool // ggml model names being fetched now, so two clients don't double-download

	settings *session.SettingKV // keyed shared-settings catalogue (whisper models, auto-compress, summary-only), synced + persisted

	rateLimitMu sync.Mutex        // guards the last-seen subscription rate-limit state
	rateLimit   session.RateLimit // account-global; cached from turns, pushed to apps on connect

	autoCompressMu sync.Mutex
	acCfg          autoCompressCfg  // global auto-compress preference (set by the app over the wire)
	acFired        map[string]int64 // session_id -> last turn `At` we auto-compressed, for dedup
}

// New builds a gateway Server. stt may be nil, in which case audio frames are
// rejected but text `utterance` messages still work. ttsClient may be nil, in
// which case `speak` requests are refused and clients use on-device TTS.
func New(cfg *config.Config, store *session.Store, hosts *session.HostStore, ids *session.IdentityStore, tokens *session.SpokenTokenStore, sshPool *session.SSHPool, driver *session.Driver, tmuxMgr *tmux.Manager, stt transcribe.Transcriber, ttsClient *tts.Client) *Server {
	var fast transcribe.Transcriber
	if cfg.WhisperFastURL != "" {
		fast = &transcribe.RemoteWhisper{URL: cfg.WhisperFastURL}
	}
	var detector detect.Detector
	if cfg.WakewordURL != "" {
		detector = &detect.RemoteWakeword{URL: cfg.WakewordURL}
		log.Printf("wakeword: detector enabled at %s (threshold %.3g)", cfg.WakewordURL, cfg.WakewordThreshold)
	}
	inflightPath, settingsPath := "", ""
	if cfg.StatePath != "" {
		inflightPath = filepath.Join(filepath.Dir(cfg.StatePath), "inflight.json")
		settingsPath = filepath.Join(filepath.Dir(cfg.StatePath), "settings_kv.json")
	}
	inflight, interrupted := newInflightTracker(inflightPath)
	settings, err := session.OpenSettingKV(settingsPath)
	if err != nil {
		log.Printf("settings: load failed (%v); using env defaults, changes won't persist", err)
		settings, _ = session.OpenSettingKV("") // in-memory fallback
	}
	// The persisted whisper model, when a user has picked one, wins over the env
	// default so a restart doesn't silently revert their choice.
	bootModel := cfg.WhisperModelName
	if m := settings.Value("whisper_model"); m != "" && validModelName(m) {
		bootModel = m
	}
	bootFast := ""
	if cfg.WhisperFastURL != "" {
		bootFast = cfg.WhisperFastModelName
		if m := settings.Value("whisper_fast_model"); m != "" && validModelName(m) {
			bootFast = m
		}
	}
	s := &Server{
		cfg:           cfg,
		store:         store,
		hosts:         hosts,
		ids:           ids,
		tokens:        tokens,
		ssh:           sshPool,
		driver:        driver,
		tmuxMgr:       tmuxMgr,
		stt:           stt,
		fastStt:       fast,
		tts:           ttsClient,
		detector:      detector,
		wakeThreshold: cfg.WakewordThreshold,
		clients:       map[string]*clientState{},
		downloading:   map[string]bool{},
		jobs:          map[string]*sessionJob{},
		conns:         map[*conn]bool{},
		inflight:      inflight,
		interrupted:   interrupted,
		acFired:       map[string]int64{},
		settings:      settings,
		currentModel:  bootModel, // persisted choice or env default; loaded below
		currentFast:   bootFast,  // ditto, for the fast (draft/detection) server
		up: websocket.Upgrader{
			// The app authenticates with a token in the hello message, so origin
			// checks add little; the network boundary (Tailscale/proxy) is the gate.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	// Load the boot model (persisted choice or env default) onto the resident
	// server so its model matches what we report to apps. Async so a big model
	// doesn't delay startup.
	if cfg.WhisperURL != "" && bootModel != "" {
		go func() {
			// Auto-fetch the boot model if the models dir is empty, so a fresh deploy
			// comes up transcribing without the operator pre-placing ggml files.
			if err := s.ensureModel(bootModel, false); err != nil {
				log.Printf("whisper: startup download failed: %v", err)
			}
			if err := s.setWhisperModel(bootModel, false); err != nil {
				log.Printf("whisper: startup load failed: %v", err)
			}
		}()
	}
	if cfg.WhisperFastURL != "" && bootFast != "" {
		go func() {
			if err := s.ensureModel(bootFast, true); err != nil {
				log.Printf("whisper: fast startup download failed: %v", err)
			}
			if err := s.setWhisperModel(bootFast, true); err != nil {
				log.Printf("whisper: fast startup load failed: %v", err)
			}
		}()
	}
	// Seed the live auto-compress preference from the persisted settings catalogue,
	// so a restart keeps whatever the user last synced (the app no longer clobbers it
	// on hello — it reconciles via the settings digest / LWW instead).
	s.applyAutoCompressFromStore()
	// Server-owned watcher that fires auto-compress near the warm-cache edge.
	go s.autoCompressLoop()
	// Server-owned watcher that notifies out loud when a detached background job
	// finishes, instead of waiting for the user's next dictation to discover it.
	go s.jobReconcileLoop()
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
	if s.tts != nil {
		// One worker per connection drains speak requests in order; closing the
		// channel (after the read loop — its handlers are the only senders) ends it,
		// and the ctx cancel aborts any in-flight synthesis.
		c.speakCh = make(chan speakReq, speakQueueLen)
		go c.speakWorker()
		defer close(c.speakCh)
	}
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
	// buffers its result for the next reconnect instead of dropping it). closed
	// is read by job sinks on other goroutines, so it rides under wmu.
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
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
		// Not the read loop: use the locked reader (the loop may be re-attaching
		// this very moment).
		sess := c.attachedSession()
		if sess == nil {
			continue
		}
		if j := s.job(sess.Name); j != nil && j.isRunning() {
			c.send(msgTurnInterrupted(sess.Name, "server restarting"))
		}
	}
}

// broadcast sends a message to every currently-connected app (best-effort; a
// failed write to a dropped socket is ignored).

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

// broadcastRenamed pushes the `renamed` title update to every connection attached
// to the just-renamed session — the initiator plus any other device the user has
// on the same session — so each client refreshes its attached-session title in
// place instead of inferring the rename from a later discovered-list diff. A
// connection is "attached to this session" when it holds the very *Session pointer
// the store renamed in place (Rename mutates the shared record, and all
// attachments resolve to it), which is precisely the session identity the app then
// matches on by the carried session_id. Connections attached to another session,
// or none, are skipped.

// broadcastRenamed pushes the `renamed` title update to every connection attached
// to the just-renamed session — the initiator plus any other device the user has
// on the same session — so each client refreshes its attached-session title in
// place instead of inferring the rename from a later discovered-list diff. A
// connection is "attached to this session" when it holds the very *Session pointer
// the store renamed in place (Rename mutates the shared record, and all
// attachments resolve to it), which is precisely the session identity the app then
// matches on by the carried session_id. Connections attached to another session,
// or none, are skipped.
func (s *Server) broadcastRenamed(rec *session.Session, old, newName string) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	msg := msgRenamed(old, newName, rec.SessionID)
	for _, c := range cs {
		if c.attachedSession() == rec {
			c.send(msg)
		}
	}
}

// job returns the session job for a name, if any.

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

// jobSink returns a sink for session-job events that reports whether it actually
// reached this client — true only if the connection is open AND the write
// succeeded. A failed write (dropped socket) returns false so the job buffers the
// result for delivery on reconnect instead of treating it as delivered and lost.
func (c *conn) jobSink() func(any) bool {
	return func(v any) bool {
		c.wmu.Lock()
		closed := c.closed
		c.wmu.Unlock()
		if closed {
			return false
		}
		return c.send(v) == nil
	}
}

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

// wireHandlers is the single registration table for the app→server control
// protocol: each inbound message `type` maps to the handler that services it.
// Adding a message means adding one entry here (and documenting it in
// docs/protocol.md — the internal/docsync drift test parses these keys and fails
// the build if any is undocumented). The hello handshake (authenticate) and
// binary audio frames (handleAudioFrame) are handled outside this table, before
// a frame ever reaches dispatch.
var wireHandlers = map[string]func(c *conn, in inbound){
	"ping":              func(c *conn, in inbound) { c.send(msgPong()) },
	"utterance":         func(c *conn, in inbound) { c.gated = false; c.handleUtterance(in.Text, in.SessionID) }, // typed/explicit text is never background-gated
	"reply":             func(c *conn, in inbound) { c.gated = false; c.handleUtterance(in.Text, in.SessionID) },
	"attach":            func(c *conn, in inbound) { c.doAttachBy(in.SessionID, in.Name, in.Silent) },
	"detach":            func(c *conn, in inbound) { c.doDetach() },
	"swap":              func(c *conn, in inbound) { c.doSwap() },
	"list_sessions":     func(c *conn, in inbound) { c.sendSessionList() },
	"discover":          func(c *conn, in inbound) { c.doDiscover() },
	"adopt":             func(c *conn, in inbound) { c.doAdopt(in.SessionID, in.Path) },
	"delete_discovered": func(c *conn, in inbound) { c.doDeleteDiscovered(in.SessionID) },
	"rename_discovered": func(c *conn, in inbound) { c.doRenameDiscovered(in.SessionID, in.Path, in.NewName) },
	"set_agent":         func(c *conn, in inbound) { c.doSetAgent(in.SessionID, in.Path, in.Agent, in.Model) },
	"rename":            func(c *conn, in inbound) { c.doRename(in.Name, in.NewName) },
	"delete":            func(c *conn, in inbound) { c.doDelete(in.Name) },
	"browse":            func(c *conn, in inbound) { c.doBrowse(in.Path, in.HostName, in.Files) },
	"upload":            func(c *conn, in inbound) { c.doUpload(in.Path, in.Name, in.HostName, in.Content) },
	"download":          func(c *conn, in inbound) { c.doDownload(in.Path, in.HostName) },
	"spawn_at": func(c *conn, in inbound) {
		c.doSpawnAt(in.Path, session.Target(in.Target), in.Create, in.HostName, in.Agent, in.Model, in.Profile, "", false)
	},
	"cancel":            func(c *conn, in inbound) { c.cancelDialog() },
	"abort":             func(c *conn, in inbound) { c.abortTurn() },
	"set_whisper_model": func(c *conn, in inbound) { c.doSetWhisperModel(in.WhisperModel, in.Fast) },
	"auto_compress": func(c *conn, in inbound) {
		c.srv.setAutoCompress(in.WarmCompress, in.AutoCompress, in.AutoCompressThreshold)
	},
	"restart":       func(c *conn, in inbound) { c.doRestart(in.Mode) },
	"speak":         func(c *conn, in inbound) { c.handleSpeak(in.ID, in.Text, in.Voice, in.Format) },
	"speak_stop":    func(c *conn, in inbound) { c.handleSpeakStop() },
	"tts_voices":    func(c *conn, in inbound) { c.handleTTSVoices() },
	"wake":          func(c *conn, in inbound) { c.startAudio(in.Codec, in.HandsFree, in.Calibrate, in.SessionID) },
	"commit":        func(c *conn, in inbound) { c.commitMessage() }, // silence-timeout commit of the hands-free buffer
	"discard_draft": func(c *conn, in inbound) { c.clearBuffer() },   // drop the uncommitted hands-free draft
	"history":       func(c *conn, in inbound) { c.serveHistory(in.Name, in.Before, in.Limit, in.HaveHash) },
	"digest":        func(c *conn, in inbound) { c.serveDigests() },
	"clear":         func(c *conn, in inbound) { c.doClear() },
	"compress":      func(c *conn, in inbound) { c.doCompress() },
	"usage":         func(c *conn, in inbound) { c.doUsage(false) }, // tap: show the report, don't speak it
	"audio_end":     func(c *conn, in inbound) { c.endAudio() },
	"hosts":         func(c *conn, in inbound) { c.sendHostList() },
	"host_put":      func(c *conn, in inbound) { c.doHostPut(in.Host) },
	"host_delete":   func(c *conn, in inbound) { c.doHostDelete(in.Name, in.UpdatedAt) },
	"identities":    func(c *conn, in inbound) { c.sendIdentityList() },
	"identity_create": func(c *conn, in inbound) {
		c.doIdentityCreate(in.Name, in.User, in.Password, in.GenKey == nil || *in.GenKey, in.UpdatedAt)
	},
	"identity_import": func(c *conn, in inbound) { c.doIdentityImport(in.Name, in.User, in.Password, in.KeyPath, in.UpdatedAt) },
	"identity_update": func(c *conn, in inbound) {
		c.doIdentityUpdate(in.Name, in.User, in.SetPassword, in.Password, in.UpdatedAt)
	},
	"identity_delete":     func(c *conn, in inbound) { c.doIdentityDelete(in.Name, in.UpdatedAt) },
	"profile_put":         func(c *conn, in inbound) { c.doProfilePut(in.ProfileDef) },
	"profile_delete":      func(c *conn, in inbound) { c.doProfileDelete(in.Name, in.UpdatedAt) },
	"profile_set_default": func(c *conn, in inbound) { c.doProfileSetDefault(in.Name) },
	"spoken_token_put":    func(c *conn, in inbound) { c.doSpokenTokenPut(in.SpokenToken) },
	"spoken_token_delete": func(c *conn, in inbound) { c.doSpokenTokenDelete(in.Name, in.UpdatedAt) },
	"provider_put":        func(c *conn, in inbound) { c.doProviderPut(in.Agent, in.DefaultModel, in.VoiceModels, in.UpdatedAt) },
	"setting_put":         func(c *conn, in inbound) { c.doSettingPut(in.Key, in.Value, in.UpdatedAt) },
}

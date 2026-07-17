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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/detect"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
	"github.com/bam/claude_spawner/server/internal/tts"
	"github.com/bam/claude_spawner/server/internal/usage"
)

// Server holds the shared dependencies for all connections.
type Server struct {
	cfg     *config.Config
	store   *session.Store
	hosts   *session.HostStore     // app-managed SSH host registry (Settings → Hosts)
	ids     *session.IdentityStore // app-managed SSH identity registry (Settings → Identities)
	ssh     *session.SSHPool       // pooled SSH connections; nil when SSH-native is disabled
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

	usage *usage.Estimator // server-global drift-live usage estimate (all sessions/clients)

	autoCompressMu sync.Mutex
	acCfg          autoCompressCfg  // global auto-compress preference (set by the app over the wire)
	acFired        map[string]int64 // session_id -> last turn `At` we auto-compressed, for dedup
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

// currentWhisperModels returns the resident servers' model names — accurate
// ("full") and fast ("quick"); the fast name is "" when no fast server is
// configured. Server-global state that apps read on connect.
func (s *Server) currentWhisperModels() (model, fastModel string) {
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	return s.currentModel, s.currentFast
}

// availableWhisperModels lists the ggml model names in cfg.WhisperModelsDir —
// the host directory the resident whisper servers mount at /models — sorted by
// file size (tiny → large), so clients can offer a picker instead of free text.
// Returns nil when the dir isn't configured (or can't be read); re-scanned on
// every call so dropping a new model file in needs no restart.
func (s *Server) availableWhisperModels() []string {
	if s.cfg.WhisperModelsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.cfg.WhisperModelsDir)
	if err != nil {
		log.Printf("whisper: list models in %s: %v", s.cfg.WhisperModelsDir, err)
		return nil
	}
	type m struct {
		name string
		size int64
	}
	var models []m
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "ggml-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		models = append(models, m{strings.TrimSuffix(strings.TrimPrefix(name, "ggml-"), ".bin"), info.Size()})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].size < models[j].size })
	names := make([]string, len(models))
	for i, mo := range models {
		names[i] = mo.name
	}
	return names
}

// catalogWhisperModels is what the picker offers: the full curated English
// catalog (small→large) followed by any extra ggml file on disk that isn't in
// it, so the app can present every downloadable English model, not just the ones
// already fetched. Returns nil when SPAWNER_WHISPER_MODELS_DIR isn't set — with
// no dir we can't download, so the app falls back to free-text entry.
func (s *Server) catalogWhisperModels() []string {
	if s.cfg.WhisperModelsDir == "" {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range transcribe.EnglishModels {
		names = append(names, m.Name)
		seen[m.Name] = true
	}
	for _, n := range s.availableWhisperModels() { // on-disk, size-ordered
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	return names
}

// ensureModel makes sure ggml-<name>.bin is present in the models dir before a
// /load, downloading it from the catalog when it's missing. It broadcasts
// progress so the app can show a download bar (a big model is a slow fetch), and
// single-flights per name so two clients selecting the same missing model share
// one download. A no-op when the file exists, the dir is unset (free-text mode),
// or the name isn't a known catalog model (let the /load surface the error).
func (s *Server) ensureModel(name string, fast bool) error {
	dir := s.cfg.WhisperModelsDir
	if dir == "" || !transcribe.IsCatalogModel(name) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, transcribe.ModelFileName(name))); err == nil {
		return nil // already present
	}
	s.downloadMu.Lock()
	if s.downloading[name] {
		s.downloadMu.Unlock()
		return fmt.Errorf("model %s is already downloading", name)
	}
	s.downloading[name] = true
	s.downloadMu.Unlock()
	defer func() {
		s.downloadMu.Lock()
		delete(s.downloading, name)
		s.downloadMu.Unlock()
	}()

	log.Printf("whisper: downloading model %s into %s", name, dir)
	s.broadcastWhisperDownload(name, fast, 0, 0, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err := transcribe.DownloadModel(ctx, dir, name, func(received, total int64) {
		s.broadcastWhisperDownload(name, fast, received, total, false, "")
	})
	if err != nil {
		s.broadcastWhisperDownload(name, fast, 0, 0, true, err.Error())
		return err
	}
	s.broadcastWhisperDownload(name, fast, 0, 0, true, "")
	return nil
}

// broadcastWhisperDownload pushes model-download progress to every connected app.
func (s *Server) broadcastWhisperDownload(name string, fast bool, received, total int64, done bool, errStr string) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(msgWhisperDownload(name, fast, received, total, done, errStr))
	}
}

// setWhisperModel hot-loads `name` onto a resident whisper server — the fast
// (draft/detection) one when fast is set, else the accurate one — and records
// it as that server's current model. Blocks on the /load; call it from a
// goroutine. name maps to /models/ggml-<name>.bin.
func (s *Server) setWhisperModel(name string, fast bool) error {
	url := s.cfg.WhisperURL
	if fast {
		url = s.cfg.WhisperFastURL
	}
	if url == "" {
		if fast {
			return fmt.Errorf("no fast whisper server configured")
		}
		return fmt.Errorf("no resident whisper server configured")
	}
	if !validModelName(name) {
		return fmt.Errorf("invalid model name %q", name)
	}
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	loaded, current := &s.whisperLoaded, &s.currentModel
	if fast {
		loaded, current = &s.fastLoaded, &s.currentFast
	}
	key := url + "|" + name
	if *loaded == key {
		*current = name
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := transcribe.LoadRemoteModel(ctx, url, "/models/ggml-"+name+".bin"); err != nil {
		return fmt.Errorf("load %s: %w", name, err)
	}
	*loaded = key
	*current = name
	if fast {
		log.Printf("whisper: fast model -> %s", name)
	} else {
		log.Printf("whisper: model -> %s", name)
	}
	return nil
}

// broadcastWhisperModel tells every connected app the current resident models
// (accurate + fast), so a change made by one client updates all of them.
func (s *Server) broadcastWhisperModel() {
	model, fastModel := s.currentWhisperModels()
	all := s.catalogWhisperModels()
	local := s.availableWhisperModels()
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(msgWhisperModel(model, fastModel, all, local))
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
// rejected but text `utterance` messages still work. ttsClient may be nil, in
// which case `speak` requests are refused and clients use on-device TTS.
func New(cfg *config.Config, store *session.Store, hosts *session.HostStore, ids *session.IdentityStore, sshPool *session.SSHPool, driver *session.Driver, tmuxMgr *tmux.Manager, stt transcribe.Transcriber, ttsClient *tts.Client) *Server {
	var fast transcribe.Transcriber
	if cfg.WhisperFastURL != "" {
		fast = &transcribe.RemoteWhisper{URL: cfg.WhisperFastURL}
	}
	var detector detect.Detector
	if cfg.WakewordURL != "" {
		detector = &detect.RemoteWakeword{URL: cfg.WakewordURL}
		log.Printf("wakeword: detector enabled at %s (threshold %.3g)", cfg.WakewordURL, cfg.WakewordThreshold)
	}
	inflightPath, usagePath, settingsPath := "", "", ""
	if cfg.StatePath != "" {
		inflightPath = filepath.Join(filepath.Dir(cfg.StatePath), "inflight.json")
		usagePath = filepath.Join(filepath.Dir(cfg.StatePath), "usage_estimate.json")
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
		usage:         usage.Open(usagePath),
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

	buffer          []string               // hands-free rough draft (per-chunk fast transcripts, for detection)
	audioPCM        []byte                 // hands-free raw PCM of all chunks, re-transcribed as one on commit
	bufferSessionID string                 // app-declared target session for the hands-free buffer
	brief           bool                   // append a "reply briefly for TTS" hint to dictation
	interactive     bool                   // let Claude ask clarifying questions mid-task
	endToken        string                 // spoken word that commits the buffer (default "beep")
	wakePhrase      [][]string             // client's custom wake token(s) (nil = built-in "hey buddy" only)
	speakPhrase     [][]string             // dictation-gate speak token(s) (nil = none); with dictationGate, only speech after it is dictated
	dictationGate   bool                   // discard un-bracketed speech instead of dictating it (needs speakPhrase set)
	sttMode         string                 // "dynamic" | "fixed" whisper model selection
	sttModel        string                 // fixed-mode model: "tiny" | "base" | "small"
	wakeService     string                 // live wake/end-token backend: "whisper" (default string-match) | "detector" (the SPAWNER_WAKEWORD_URL sidecar)
	aliases         map[string]string      // mis-transcription -> canonical command word
	scratch         bool                   // scratch mode: while detached, echo each transcription back aloud (STT test)

	speakCh     chan speakReq      // queued `speak` requests, drained in order by speakWorker; nil = server TTS disabled
	speakMu     sync.Mutex         // guards speakCancel (read loop vs speak worker)
	speakCancel context.CancelFunc // aborts the in-flight synthesis (speak_stop); nil = none running
}

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
func (c *conn) attachedSession() *session.Session {
	c.attachedMu.Lock()
	defer c.attachedMu.Unlock()
	return c.attached
}

// transcriber returns this connection's STT — the server default (the whisper
// server is fixed by config; clients no longer override the URL).
func (c *conn) transcriber() transcribe.Transcriber {
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
	// Auto-compress is no longer clobbered from hello — it's a synced setting now,
	// reconciled below via the settings digest / last-writer-wins, so a fresh client
	// doesn't stomp a preference another client set. See the settings catalogue block.
	c.endToken = strings.TrimSpace(in.EndToken)
	if c.endToken == "" {
		c.endToken = "beep"
	}
	c.wakePhrase = command.WakePhrase(in.WakeToken)
	c.speakPhrase = command.WakePhrase(in.SpeakToken)
	c.dictationGate = in.DictationGate
	c.sttMode = in.SttMode
	c.sttModel = in.SttModel
	// Live wake/end-token backend. Default to the always-present Whisper string-match
	// so a fresh/older client never lands on the trained sidecar implicitly — the
	// detector is opt-in per client (the app's toggle), even when SPAWNER_WAKEWORD_URL
	// is configured server-wide. Anything other than "detector" means Whisper.
	c.wakeService = strings.TrimSpace(in.WakeService)
	c.aliases = in.Aliases
	// The whisper model is server-global: the app reads it here rather than pushing
	// its own (so two clients don't bounce it), and changes it via set_whisper_model.
	model, fastModel := c.srv.currentWhisperModels()
	c.send(msgHelloOK("ws", model, fastModel, c.srv.catalogWhisperModels(), c.srv.availableWhisperModels(), c.srv.tts != nil))
	// Per-catalogue digest fast path (skip-if-equal): the app presents a digest of
	// each cached catalogue in `hello`; we re-send only the ones whose digest differs
	// from ours, so an unchanged catalogue costs nothing on connect (Phase 2a LWW
	// resolves direction on the ones we do send). Mirrors the chat transcript's
	// count+hash `digests`/`history unchanged` shortcut. An older client sends no
	// digest → the "" mismatch makes every catalogue ship, so it's fully backward
	// compatible. See catalogdigest.go.
	//
	// Advertise the AI backend registry so the app's new-session picker can offer a
	// backend + model choice (and badge sessions by backend). Also kick a throttled
	// live re-discovery in the background: models added to a backend since boot land
	// in every connected app moments later, no restart needed.
	reg := c.srv.driver.Registry()
	provSettings := c.srv.driver.ProviderSettings()
	if in.ProvidersDigest != providersDigest(reg, provSettings) {
		c.send(msgAgents(reg, provSettings))
	}
	go c.srv.refreshModelsOnConnect()
	// Advertise execution profiles separately from hello_ok so older clients can
	// ignore the message and still use the built-in default profile.
	profReg := c.srv.driver.ProfileRegistry()
	if in.ProfilesDigest != profilesDigest(profReg.List()) {
		c.send(msgProfiles(profReg))
	}
	// Hosts and identities were previously request-only; presenting a digest lets us
	// proactively reconcile them on connect too, so a different client's edit reflects
	// here without opening the settings screen — but only when they actually differ.
	if in.HostsDigest != hostsDigest(c.srv.hosts.List()) {
		c.send(msgHostList(c.srv.hosts.List()))
	}
	if in.IdentitiesDigest != identitiesDigest(c.srv.ids.List()) {
		c.send(msgIdentityList(c.srv.ids.List()))
	}
	// The fifth catalogue: genuinely-shared server-global scalars (whisper models,
	// auto-compress, summary-only). Same skip-if-equal fast path — re-send only when
	// the app's digest differs; LWW on the records reconciles direction on the ones
	// we do send.
	if in.SettingsDigest != settingsDigest(c.srv.settings) {
		c.send(msgSettings(c.srv.settings.List()))
	}
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
	"usage":         func(c *conn, in inbound) { c.doUsage(false, usageCalibrate) }, // tap: show the report, don't speak it
	"usage_set":     func(c *conn, in inbound) { c.doUsage(false, usageSetBench) },  // "set" button: arm the benchmark
	"usage_calc":    func(c *conn, in inbound) { c.doUsage(false, usageCalcBench) }, // "calc" button: derive the rate
	"audio_end":     func(c *conn, in inbound) { c.endAudio() },
	"hosts":         func(c *conn, in inbound) { c.sendHostList() },
	"host_put":      func(c *conn, in inbound) { c.doHostPut(in.Host) },
	"host_delete":   func(c *conn, in inbound) { c.doHostDelete(in.Name, in.UpdatedAt) },
	"identities":    func(c *conn, in inbound) { c.sendIdentityList() },
	"identity_create": func(c *conn, in inbound) {
		c.doIdentityCreate(in.Name, in.User, in.Password, in.GenKey == nil || *in.GenKey, in.UpdatedAt)
	},
	"identity_import":     func(c *conn, in inbound) { c.doIdentityImport(in.Name, in.User, in.Password, in.KeyPath, in.UpdatedAt) },
	"identity_update":     func(c *conn, in inbound) { c.doIdentityUpdate(in.Name, in.User, in.SetPassword, in.Password, in.UpdatedAt) },
	"identity_delete":     func(c *conn, in inbound) { c.doIdentityDelete(in.Name, in.UpdatedAt) },
	"profile_put":         func(c *conn, in inbound) { c.doProfilePut(in.ProfileDef) },
	"profile_delete":      func(c *conn, in inbound) { c.doProfileDelete(in.Name, in.UpdatedAt) },
	"profile_set_default": func(c *conn, in inbound) { c.doProfileSetDefault(in.Name) },
	"provider_put":        func(c *conn, in inbound) { c.doProviderPut(in.Agent, in.DefaultModel, in.VoiceModels, in.UpdatedAt) },
	"setting_put":         func(c *conn, in inbound) { c.doSettingPut(in.Key, in.Value, in.UpdatedAt) },
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

// stripWake / splitWake are the connection-scoped wake matchers: they honor this
// client's custom wake token (c.wakePhrase) in addition to the built-in "hey
// buddy" family. Use these instead of the package-level command.StripWake /
// command.SplitWake anywhere a *conn is in scope.
func (c *conn) stripWake(text string) (rest string, hadWake bool) {
	return command.StripWakeWith(text, c.wakePhrase)
}

func (c *conn) splitWake(text string) (before, after string, found bool) {
	return command.SplitWakeWith(text, c.wakePhrase)
}

func (c *conn) splitWakeAll(text string) (before string, commands []string) {
	return command.SplitWakeAllWith(text, c.wakePhrase)
}

// gateDictation applies the dictation gate to a would-be dictation string. When
// the gate is on (and a speak token is configured), only the text following the
// speak token is dictated (the token stripped); text with no speak token returns
// "" so the caller drops it as ambient chatter. Gate off — or no speak token —
// passes text through unchanged, preserving the ungated behavior. Commands are
// never routed through here, so "hey buddy stop" always works regardless.
func (c *conn) gateDictation(text string) string {
	if !c.dictationGate || len(c.speakPhrase) == 0 {
		return text
	}
	if _, after, found := command.SplitOn(text, c.speakPhrase); found {
		return strings.TrimSpace(after)
	}
	return ""
}

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

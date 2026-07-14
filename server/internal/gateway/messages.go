package gateway

import (
	"time"

	"github.com/bam/claude_spawner/server/internal/agent"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/usage"
)

// Wire types for the WebSocket protocol (see docs/protocol.md). Inbound JSON is
// decoded into `inbound`; outbound messages are plain maps built by the helpers
// below so each carries only the fields it needs.

const serverVersion = "0.1.0"

// inbound is the union of fields any app->server message may carry.
type inbound struct {
	Type                  string               `json:"type"`
	Token                 string               `json:"token"`
	Text                  string               `json:"text"`                    // utterance / dialog reply text
	Name                  string               `json:"name"`                    // session name for attach/kill/rename
	NewName               string               `json:"new_name"`                // target name for rename
	Path                  string               `json:"path"`                    // directory for browse / spawn_at ("" on browse = the host's root "/"); file path for download
	Files                 bool                 `json:"files"`                   // on browse: include regular files in the listing (file-transfer picker), not just directories
	Content               string               `json:"content"`                 // on upload: the file's bytes, base64-encoded
	Target                string               `json:"target"`                  // on spawn_at: "host" (default) | "sandbox" execution target
	Create                bool                 `json:"create"`                  // on spawn_at: mkdir the path (on the target host) first if it doesn't exist
	Agent                 string               `json:"agent"`                   // on spawn_at/set_agent: AI backend id ("codex"); "" = default backend
	Model                 string               `json:"model"`                   // on spawn_at/set_agent: model alias for the session; "" = the backend's default
	DefaultModel          string               `json:"default_model"`           // on provider_put: the backend's overridden default model alias ("" = its compiled default)
	VoiceModels           []string             `json:"voice_models"`            // on provider_put: the exact model aliases the voice commands enumerate (nil = leave at all)
	Profile               string               `json:"profile"`                 // on spawn_at: execution profile name; "" = default profile
	ProfileDef            *session.ExecProfile `json:"profile_def"`             // on profile_put: the full execution profile to add/update
	Codec                 string               `json:"codec"`                   // audio codec on wake: "ogg_opus" | "pcm16"
	ClientID              string               `json:"client_id"`               // stable per-app id, for reconnect/resume
	HandsFree             bool                 `json:"hands_free"`              // set on `wake` when the clip is VAD-gated (hands-free)
	EndToken              string               `json:"end_token"`               // on `hello`: the spoken word that commits a message
	WakeToken             string               `json:"wake_token"`              // on `hello`: custom wake word(s), comma-separated, accepted alongside built-in "hey buddy" ("" = built-in only)
	SpeakToken            string               `json:"speak_token"`             // on `hello`: dictation-gate start marker(s), comma-separated; only speech after it (up to the end token) is dictated ("" = no gate token)
	DictationGate         bool                 `json:"dictation_gate"`          // on `hello`: when true (and a speak token is set), un-bracketed speech is discarded instead of dictated — ambient-chatter immunity
	SttMode               string               `json:"stt_mode"`                // on `hello`: "dynamic" | "fixed"
	SttModel              string               `json:"stt_model"`               // on `hello`: fixed model "tiny" | "base" | "small"
	Calibrate             bool                 `json:"calibrate"`               // on `wake`: transcribe (fast model) and return, don't dispatch
	Aliases               map[string]string    `json:"aliases"`                 // on `hello`: mis-transcription -> canonical command word
	WhisperURL            string               `json:"whisper_url"`             // on `hello`: resident whisper server URL (overrides the default)
	WhisperModel          string               `json:"whisper_model"`           // on `hello`: ggml model to hot-load on the resident server (e.g. "medium.en")
	Fast                  bool                 `json:"fast"`                    // on `set_whisper_model`: target the fast (draft/detection) server instead of the accurate one
	Rebuild               *bool                `json:"rebuild"`                 // on `restart`: recompile from source (nil/absent = yes, back-compat) vs a fast bounce that recreates from the existing image
	Before                *int                 `json:"before"`                  // on `history`: page cursor (exclusive index); nil = most recent
	Limit                 int                  `json:"limit"`                   // on `history`: page size (default 30)
	HaveHash              string               `json:"have_hash"`               // on `history`: digest of the top page the app already cached; server replies `unchanged` if it still matches
	Silent                bool                 `json:"silent"`                  // on `attach`: suppress the spoken "attached…" confirmation (reconnect auto-attach)
	SessionID             string               `json:"session_id"`              // on `adopt`: the discovered Claude session_id to register
	Brief                 bool                 `json:"brief"`                   // on `hello`: append a "reply briefly for TTS" hint to dictation
	Interactive           bool                 `json:"interactive"`             // on `hello`: let Claude ask clarifying questions mid-task
	WarmCompress          bool                 `json:"warm_compress"`           // on `hello`/`auto_compress`: compress a session in the last seconds of its warm-cache window
	AutoCompress          bool                 `json:"auto_compress"`           // on `hello`/`auto_compress`: compress a session immediately once it crosses the limit
	AutoCompressThreshold int                  `json:"auto_compress_threshold"` // on `hello`/`auto_compress`: context-token limit, in thousands (shared by warm + auto)
	Host                  *session.Host        `json:"host"`                    // on `host_put`: the SSH host entry to add/update
	HostName              string               `json:"host_name"`               // on `browse`/`spawn_at`: which registered SSH host to browse / run the new session on ("" = local)
	KeyPath               string               `json:"key_path"`                // on `identity_import`: server-side path of the existing private key to register
	User                  string               `json:"user"`                    // on `identity_create`/`identity_import`: the identity's default SSH login user (required)
	Password              string               `json:"password"`                // on `identity_create`/`identity_import`: optional SSH password (server-only)
	GenKey                *bool                `json:"gen_key"`                 // on `identity_create`: generate a keypair (nil = yes, for older clients)
	SetPassword           bool                 `json:"set_password"`            // on `identity_update`: apply Password (else keep the current one)
	ID                    string               `json:"id"`                      // on `speak`: client-chosen correlation id, echoed on speak_audio/speak_end
	Voice                 string               `json:"voice"`                   // on `speak`: Kokoro voice override ("" = the server default, SPAWNER_TTS_VOICE)
	Format                string               `json:"format"`                  // on `speak`: response-format override ("" = the server default, SPAWNER_TTS_FORMAT)
}

// msgAgents advertises the AI backend registry to the app so the visual
// new-session picker and the Providers settings tab can offer a backend and model
// choice: each backend's id, display name, its *effective* default model alias
// (the user's override from the provider-settings overlay, else the compiled
// default), and its selectable models. Each model carries its `alias` and a
// `voice` flag — whether the voice "list models" / "use model N" commands
// enumerate it (the user toggles this per model in the Providers tab). `default`
// is the backend a spawn gets when none is chosen. The provider overlay is
// nil-safe: with no overrides, defaults are compiled and every model is voice-on.
func msgAgents(reg *agent.Registry, settings *agent.SettingsStore) map[string]any {
	agents := make([]map[string]any, 0)
	for _, a := range reg.List() {
		cat := a.Catalog()
		models := make([]map[string]any, 0, len(cat))
		for _, m := range cat {
			models = append(models, map[string]any{"alias": m.Alias, "voice": settings.VoiceEnabled(a, m.Alias)})
		}
		agents = append(agents, map[string]any{
			"id": a.ID, "name": a.Name, "default_model": settings.DefaultModel(a), "models": models,
		})
	}
	def := ""
	if d := reg.Default(); d != nil {
		def = d.ID
	}
	return map[string]any{"type": "agents", "agents": agents, "default": def}
}

// msgProfiles advertises the execution-profile catalogue. Each entry is the full
// ExecProfile (so the app's profiles editor can round-trip every field); the
// top-level `default` names the marked-default profile for convenience. The
// ExecProfile structs are marshaled directly, so their JSON tags are the wire shape.
func msgProfiles(reg *session.ProfileRegistry) map[string]any {
	profiles := reg.List()
	if profiles == nil {
		profiles = []*session.ExecProfile{}
	}
	return map[string]any{"type": "profiles", "profiles": profiles, "default": reg.DefaultName()}
}

func msgHelloOK(sessionID, whisperModel, whisperFastModel string, whisperModels, whisperModelsLocal []string, tts bool) map[string]any {
	if whisperModels == nil {
		whisperModels = []string{}
	}
	if whisperModelsLocal == nil {
		whisperModelsLocal = []string{}
	}
	return map[string]any{
		"type": "hello_ok", "server_version": serverVersion, "session_id": sessionID,
		"whisper_model": whisperModel, "whisper_model_fast": whisperFastModel,
		"whisper_models": whisperModels, "whisper_models_local": whisperModelsLocal,
		"tts": tts,
	}
}

// msgWhisperModel reports the resident whisper servers' current models (server-
// global): the accurate one and the fast draft/detection one (fast_model is ""
// when no fast server is configured). `whisper_models` is the full English
// catalog offered as a picker (plus any extra on-disk ggml file), and
// `whisper_models_local` is the subset actually downloaded — so the app can mark
// which entries would download on select. Both empty when
// SPAWNER_WHISPER_MODELS_DIR isn't set. Broadcast on any change.
func msgWhisperModel(name, fastName string, models, local []string) map[string]any {
	if models == nil {
		models = []string{}
	}
	if local == nil {
		local = []string{}
	}
	return map[string]any{
		"type": "whisper_model", "model": name, "fast_model": fastName,
		"whisper_models": models, "whisper_models_local": local,
	}
}

// msgWhisperDownload reports progress of an on-demand ggml model download (the
// server fetches a catalog model that isn't on disk when a client selects it).
// `received`/`total` are bytes (total 0 = unknown); `done` marks completion, and
// `error` is set (with done) when the download failed. `fast` echoes which
// server the download is for. Broadcast to every client so all see the progress.
func msgWhisperDownload(model string, fast bool, received, total int64, done bool, errStr string) map[string]any {
	return map[string]any{
		"type": "whisper_download", "model": model, "fast": fast,
		"received": received, "total": total, "done": done, "error": errStr,
	}
}

func msgSay(text string) map[string]any {
	return map[string]any{"type": "say", "text": text}
}

// msgHostList carries the full SSH host registry (Settings → Hosts). Sent to the
// requester on `hosts` and broadcast to every client after a host_put/host_delete,
// so the app-managed list stays in sync across clients.
func msgHostList(hosts []*session.Host) map[string]any {
	if hosts == nil {
		hosts = []*session.Host{}
	}
	return map[string]any{"type": "host_list", "hosts": hosts}
}

// msgIdentityList carries the SSH identity registry (Settings → Identities): each
// entry's name, user, PUBLIC key, and whether a password is set — never the private
// key or the password itself. Sent to the requester on `identities` and broadcast
// after identity_create/identity_import/identity_delete.
func msgIdentityList(ids []*session.Identity) map[string]any {
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]any{
			"name":         id.Name,
			"user":         id.User,
			"public_key":   id.PublicKey,
			"has_password": id.Password != "",
		})
	}
	return map[string]any{"type": "identity_list", "identities": out}
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

// msgTranscribing signals that a committed hands-free clip is now being
// re-transcribed accurately (the window between the draft clearing and the
// transcript landing), so the app can show "transcribing…" instead of snapping
// back to "listening". Payload-free; superseded by the transcript that follows.
func msgTranscribing() map[string]any {
	return map[string]any{"type": "transcribing"}
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
func msgAttached(s *session.Session, cx *session.ContextSnapshot) map[string]any {
	m := map[string]any{"type": "attached", "name": s.Name, "session_id": s.SessionID}
	// The backend + current model, so the app can show which AI (and which model)
	// this session runs. Empty for records predating backend selection.
	if s.Agent != "" {
		m["agent"] = s.Agent
	}
	if s.Model != "" {
		m["model"] = s.Model
	}
	if s.Profile != "" {
		m["profile"] = s.Profile
	}
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
// names — plus the stable `session_id` — so the app can update the
// attached-session title in place, matching by id (names diverge across servers),
// without the heavy re-attach side effects (history refetch, context-meter
// reseed) that a fresh `attached` message would trigger. Only sent when the
// rename follows the connection's attached session.
func msgRenamed(old, name, sessionID string) map[string]any {
	return map[string]any{"type": "renamed", "old": old, "name": name, "session_id": sessionID}
}

// msgOutput carries session output for display + TTS. Live prose streams as
// chunk=true messages; the final chunk=false message closes the turn and (only
// then) carries the turn's token `usage`, which the app renders as a per-message
// badge. usage is nil for streaming chunks (no per-chunk accounting exists).
// The final message is stamped with `usage_at` (this turn's completion time) so
// a client that receives it buffered on reconnect anchors its cache-warm
// countdown to the turn's real age, not to when the message finally arrived.
func msgOutput(name, text string, chunk bool, usage *session.Usage) map[string]any {
	m := map[string]any{"type": "output", "name": name, "text": text, "chunk": chunk}
	if usage != nil {
		m["usage"] = usage
		m["usage_at"] = time.Now().Unix()
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

// msgSpeakAudio heads one synthesized utterance (response to `speak`): the
// binary frames that follow, up to the matching speak_end, are its audio in
// the named codec (SPAWNER_TTS_FORMAT: "opus" | "mp3" | …). Speaks are
// serviced one at a time per connection, so streams never interleave.
func msgSpeakAudio(id, codec string) map[string]any {
	return map[string]any{"type": "speak_audio", "id": id, "codec": codec}
}

// msgSpeakEnd closes a speak stream. `error` is "" on success; non-empty when
// synthesis failed or was refused (tts disabled, empty text, queue full) — the
// client should fall back to on-device TTS for that utterance.
func msgSpeakEnd(id, errStr string) map[string]any {
	return map[string]any{"type": "speak_end", "id": id, "error": errStr}
}

// msgTTSVoices relays Kokoro's voice catalogue (reply to `tts_voices`): the
// selectable voice ids plus the server-default voice (SPAWNER_TTS_VOICE) the
// client's picker shows as "server default". error non-empty (tts disabled,
// voices unavailable) = no catalogue; the picker stays free-defaulted.
func msgTTSVoices(voices []string, def, errStr string) map[string]any {
	if voices == nil {
		voices = []string{}
	}
	return map[string]any{"type": "tts_voices", "voices": voices, "default": def, "error": errStr}
}

// msgSpeechMode tells the app whether to speak only the final result of a turn
// (summary_only true: intermediate streamed steps beep instead of being read
// aloud) or everything (false). Sent by the "summary only" / "speak everything"
// voice commands; the app's audio settings has the same switch.
func msgSpeechMode(summaryOnly bool) map[string]any {
	return map[string]any{"type": "speech_mode", "summary_only": summaryOnly}
}

// msgHistory returns a page of a session's past conversation (older-to-newer),
// with `more` telling the app whether even-older messages remain to page in.
// `count`/`hash` are the whole chain's digest (the app stores them alongside the
// cached transcript); `unchanged` is set on a top-page request whose `have_hash`
// still matched, meaning the app's cache is current and `messages` is empty.
func msgHistory(name string, messages []session.Message, more bool, count int, hash string, unchanged bool) map[string]any {
	if messages == nil {
		messages = []session.Message{}
	}
	return map[string]any{
		"type": "history", "name": name, "messages": messages, "more": more,
		"count": count, "hash": hash, "unchanged": unchanged,
	}
}

// digestView is one session's transcript summary in the `digests` message: the
// message count and an opaque content hash the app compares against its cached
// copy to decide whether that session's history changed and needs refetching.
type digestView struct {
	Name      string `json:"name"`
	SessionID string `json:"session_id"`
	Count     int    `json:"count"`
	Hash      string `json:"hash"`
}

// msgDigests reports every session's transcript digest (count + content hash) so
// the app can validate its offline cache on connect without pulling any message
// bodies — it refetches history only for sessions whose hash no longer matches.
func msgDigests(items []digestView) map[string]any {
	if items == nil {
		items = []digestView{}
	}
	return map[string]any{"type": "digests", "items": items}
}

// discoveredView is a Claude session found on disk (see session.Discovered),
// annotated with whether it's already in the registry and whether it looks live
// in tmux (adopting + driving it then risks a two-writer conflict).
type discoveredView struct {
	Name       string `json:"name"`
	Dir        string `json:"dir"`
	SessionID  string `json:"session_id"`
	LastActive int64  `json:"last_active"`      // unix seconds
	Active     bool   `json:"active"`           // interactive claude open in tmux at this dir
	Registered bool   `json:"registered"`       // already in the spawner registry
	Busy       bool   `json:"busy"`             // a dictation turn is running for this session now
	Target     string `json:"target,omitempty"` // execution target ("sandbox") when not the default host
	Host       string `json:"host,omitempty"`   // the SSH host the session runs on (for grouping in the app)
	Agent      string `json:"agent,omitempty"`  // AI backend id ("codex") when not the default; badge in the app
	Model      string `json:"model,omitempty"`  // current model alias for the session
	Profile    string `json:"profile,omitempty"`
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
	// Agent is the AI backend id ("claude" | "codex") the session runs, so the app
	// can badge non-default backends. Model is its current model alias. Both may be
	// empty for records predating backend selection (→ the default backend/model).
	Agent string `json:"agent,omitempty"`
	Model string `json:"model,omitempty"`
	// Profile is the execution profile name when not the built-in default.
	Profile string `json:"profile,omitempty"`
}

// listingEntry is one entry in a browse listing.
type listingEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Repo bool   `json:"repo"` // true if it's a git repo
	Dir  bool   `json:"dir"`  // true for a directory; false for a regular file (only files-mode browse returns files)
}

// msgListing is the response to a `browse`: the directory's entries, plus the
// parent path for "up" ("" means the parent is the roots view).
func msgListing(path, parent string, entries []listingEntry) map[string]any {
	return map[string]any{"type": "listing", "path": path, "parent": parent, "entries": entries}
}

// msgFileSaved confirms an `upload` landed: path is the file's absolute location on
// the target host, which the app uses to prefill the message box.
func msgFileSaved(path string) map[string]any {
	return map[string]any{"type": "file_saved", "path": path}
}

// msgFileData is the response to a `download`: the file's bytes (base64) plus its
// name and source path so the app can offer a "save as" with a sensible default.
func msgFileData(name, path, content string) map[string]any {
	return map[string]any{"type": "file_data", "name": name, "path": path, "content": content}
}

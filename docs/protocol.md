# WebSocket protocol

The wire protocol between the Android app and the Go server. One authenticated WebSocket per app
connection carries everything: control messages, audio frames up, transcripts and session output
down. This doc is the single source of truth for both ends.

## Connection & auth

1. App opens `wss://<host>/ws`.
2. App sends a `hello` message with an auth token within the handshake window.
3. Server replies `hello_ok` or closes with a `4401` (unauthorized) code.

```jsonc
// app -> server
{ "type": "hello", "token": "<shared-secret-or-jwt>", "client_id": "<stable-uuid>" }
// server -> app
{ "type": "hello_ok", "server_version": "0.1.0", "session_id": "ws-abc123" }
```

`client_id` is a stable per-install UUID. On reconnect the server resumes an in-progress spawn
dialog for that client (re-emitting the current prompt). Re-attaching to a session is client-driven:
the app remembers the last attached session name and re-sends `attach` after `hello_ok` — which
also works across a server restart, since sessions are durable on disk. The client auto-reconnects
with backoff and auto-connects on launch.

All subsequent messages are JSON text frames **except audio**, which uses binary frames.

## Message envelope

Every JSON message has a `type`. Optional `id` correlates request/response. `ts` is server time
(ms epoch) on server-originated messages.

## App -> server

| type            | payload                                  | meaning                                       |
|-----------------|------------------------------------------|-----------------------------------------------|
| `hello`         | `token`, `client_id`                     | auth handshake (plus the optional flags listed below the table) |
| `wake`          | `{ "codec": "ogg_opus", "hands_free?": false, "calibrate?": false, "session_id?": "<uuid>" }` | audio capture starts; binary audio frames will follow. `session_id` is the app's currently focused session and, when present, is the dictation target for this clip / hands-free buffer (validated by the server; omitted = legacy per-connection attached session). |
| *(binary)*      | raw PCM/Opus frames                       | audio chunk (between `wake` and `audio_end`)  |
| `audio_end`     | `{}`                                      | end of utterance; server finalizes transcript |
| `utterance`     | `{ "text": "<what the user said>", "session_id?": "<uuid>" }` | **the text seam** — a complete utterance as text (post-STT or typed). `session_id`, when present, is the app's focused session and is validated/used as the dictation target before falling back to legacy per-connection attachment. |
| `reply`         | `{ "text": "<user reply>", "session_id?": "<uuid>" }` | alias of `utterance` for dialog replies       |
| `attach`        | `{ "session_id"?: "<uuid>", "name"?: "<session>", "silent": false }`| request attach. Prefer `session_id` (the stable handle — survives renames and is the same session across servers); the server resolves it to the current name, falling back to `name` if the id is unknown or absent. `silent: true` suppresses the spoken "attached… go ahead, bud." confirmation (used for the app's auto re-attach on reconnect); a finished turn's buffered result is still delivered. |
| `detach`        | `{}`                                      | leave passthrough                             |
| `swap`          | `{}`                                      | toggle back to the previously attached session — a two-way jump between the two most-recent sessions for this connection. The server tracks the previous `session_id` per connection (set on each genuine attach, and on detach); `swap` attaches to it and records the outgoing session as the new previous, so repeated swaps ping-pong. Speaks "no previous session…" when there's nothing to swap to. Same handler as the voice **swap** command; the app also fires it from a right-to-left swipe on the chat. -> `attached` |
| `list_sessions` | `{}`                                      | request the session list (quiet; for the sidebar) -> `session_list` |
| `discover`      | `{}`                                      | scan `~/.claude/projects` for ALL Claude sessions (spawner-created or not, e.g. interactive `claude` in tmux) -> `discovered` |
| `adopt`         | `{ "session_id": "<uuid>", "path": "<dir>" }` | register a discovered session into the store and attach to it (so the app can view/drive it via `--resume`) -> `attached` + `session_list`. The `session_id` is the sole identity: if it's already registered, attach to that record; otherwise adopt it as a new session even when the folder already hosts a different one (its name simply dedups to `<dir>-2`) — a directory is only a working dir, not an identity |
| `delete_discovered` | `{ "session_id": "<uuid>" }`          | PERMANENTLY delete ONE session — its transcript(s) (the current `session_id` plus any rotated prior ids) and its single registry record — targeted by id, so its dir-mates are left intact. Refused with `session_active` if the directory is live in a terminal. -> refreshed `discovered` + `session_list` |
| `rename_discovered` | `{ "session_id": "<uuid>", "path": "<dir>", "new_name": "<name>" }` | give a discovered session a custom name, resolving the target by `session_id` (registers it — by `path` if not yet in the store — without attaching). -> refreshed `discovered` + `session_list` |
| `set_agent`     | `{ "session_id": "<uuid>", "path": "<dir>", "agent": "", "model": "" }` | switch a session's AI backend + model durably (from the sidebar Edit dialog), resolving the target by `session_id` (registers a still-discovered one by `path` first, like `rename_discovered`). `agent` is a backend id from the `agents` message (`""` = the default backend); `model` a model alias for it (`""`/unknown = that backend's default). **Changing the backend rotates the session to a fresh `session_id` and un-starts it** — Claude and Codex transcripts use incompatible on-disk formats, so a switch begins a clean conversation on the new AI (the old transcript stays on disk, off this chain); refused with `busy` if a turn is in flight. Changing only the model keeps the conversation. -> refreshed `discovered` + `session_list` (+ `attached` when the switched session is the one you're attached to). |
| `rename`        | `{ "name": "<old>", "new_name": "<new>" }`| rename a session (keeps its session_id) -> `session_list` |
| `delete`        | `{ "name": "<session>" }`                 | delete a session record -> `session_list`     |
| `browse`        | `{ "path": "<dir or empty>", "host_name": "", "files": false }` | list a directory **on `host_name`** (over SSH; `""` = local/loopback) for the New-session picker. Empty `path` = that host's filesystem root `/`. There is no spawn jail — the picker walks the whole host. `files` optional (default `false`): when `true` the listing also includes regular files (each entry's `dir` flag distinguishes them) for the file-transfer picker; otherwise only subdirectories are returned. -> `listing` |
| `upload`        | `{ "path": "<dest dir>", "name": "<filename>", "host_name": "", "content": "<base64>" }` | write an uploaded file to `name` inside directory `path` **on `host_name`** (over SSH; `""` = local/loopback). `content` is the file's bytes, base64-encoded, in one message (capped at 64 MiB → `file_too_large`). `name` is reduced to its basename so it can't escape `path`. -> `file_saved` |
| `download`      | `{ "path": "<file path>", "host_name": "" }` | read the file at absolute `path` **on `host_name`** (over SSH; `""` = local/loopback) and return its bytes (capped at 64 MiB → `file_too_large`). -> `file_data` |
| `spawn_at`      | `{ "path": "<dir>", "target": "host\|sandbox", "create": false, "host_name": "", "agent": "", "model": "", "profile": "" }` | open `path` **on `host_name`**: always create a NEW session there and attach to it — a directory is only the session's initial working dir, never its identity, so spawning into a folder that already has a session mints a fresh one (its name dedups to `<dir>-2`) rather than re-attaching. Re-attaching to an existing session is `attach`'s job. -> `attached` + `session_list`. `path` must be absolute. `target` optional (default `host`); `sandbox` runs turns in an isolated container and errors if no sandbox image is configured. `create` optional (default `false`): when `true`, `mkdir` the `path` on the target host first so the picker can start a project in a directory that doesn't exist yet — errors `bad_path` if the folder already exists. `host_name` optional (default `""` = local): the registered SSH host (Settings → Hosts) to browse / run the session on; ignored for `sandbox`. `agent` optional (default `""` = default backend): the AI backend id (from the `agents` message) the session runs. `model` optional (default `""` = the backend's default): a model alias from that backend; an unknown model falls back to the default. `profile` optional: an execution-profile name from the `profiles` message; empty or unknown resolves to the default-marked profile (else the first). When `target` is omitted and the selected profile has an advisory `target`, the server uses that target. |
| `history`       | `{ "name": "<session>", "before": <int?>, "limit": <int>, "have_hash?": "<hex>" }` | request a page of that session's past conversation (from Claude's transcript). `before` = exclusive index cursor (omit for the most recent page; page older by passing the oldest index held). Spans context rotations: after a `clear`, the retired transcripts and the current one are stitched into one continuous, contiguously-indexed conversation. `have_hash` (optional; only meaningful on a top-page request, i.e. `before` omitted) is the digest of the transcript the app already cached — when it still matches the current chain the server replies with `unchanged: true` and no message bodies, so reopening an unchanged session transfers nothing. -> `history` |
| `digest`        | `{}`                                      | request every registered session's transcript digest (message `count` + content `hash`) so the app can validate its offline transcript cache without transferring any message bodies; sent on connect. -> `digests` |
| `clear`         | `{}`                                      | rotate the attached session's Claude context: retire the current `session_id` (its transcript kept for `history`) and start a fresh one, so the next dictation replays no prior context. No model tokens spent; history still spans the whole chain. -> `context_reset` + `say` |
| `compress`      | `{}`                                      | compact the attached session's Claude context: run a background turn asking Claude to summarize the conversation, then rotate the `session_id` (old transcript kept for `history`) and carry the summary forward — it seeds the next dictation so Claude continues with the condensed context instead of dropping it (the `/compact` analogue of `clear`). Emits an `activity` breadcrumb while summarizing, then a `context_reset` (readout back to zero) and a `say`. Refused if a turn is in flight (`say`) or no turn has run yet (`say`). -> `activity` + `context_reset` + `say` |
| `cancel`        | `{}`                                      | abort current dialog                          |
| `usage`         | `{}`                                      | fetch the Claude plan's usage report by running `claude -p "/usage"` headless (a real but lightweight invocation) -> `usage`. The "usage" voice command does the same but also speaks a summary. |
| `usage_set`     | `{}`                                      | arm a manual two-point rate benchmark: read `/usage`, then stamp the current odometer position and real percentages as the start mark (the app's **"set"** button). -> `usage` + `usage_estimate` + `say`. |
| `usage_calc`    | `{}`                                      | close the manual benchmark: read `/usage`, then set each window's tokens-per-percent rate **directly** from `(tokens since the `usage_set` mark) / (percent gained)` — no EMA damping — and re-anchor the estimate (the app's **"calc"** button). A window that moved less than 1% since the mark is left unchanged. -> `usage` + `usage_estimate` + `say`. |
| `abort`         | `{}`                                      | cancel the running dictation turn on the attached session (kills the claude child) -> `turn_stopped` |
| `set_whisper_model` | `{ "whisper_model": "<name>", "fast": false }` | hot-load a model on a resident whisper server, server-global: the accurate ("full" transcribe) server by default, or the fast draft/detection ("quick" transcribe) server when `fast` is `true`. Fans out a `whisper_model` broadcast to every connected client; errors `whisper_failed` if that server isn't configured or the load fails. |
| `auto_compress`  | `{ "warm_compress": <bool>, "auto_compress": <bool>, "auto_compress_threshold": <int> }` | set the server-global **context-compression** preference live (the same three fields the `hello` handshake carries). Two independent triggers share one limit — once a started session's context exceeds `auto_compress_threshold` **thousand** tokens (measured as `input + cache_write + cache_read`, matching the app's context badge): **`warm_compress`** auto-runs a `compress` in the last ~15 s of that session's 5-minute warm-cache window (so the summary turn reuses the still-warm cache instead of paying a cold rebuild later); **`auto_compress`** compresses immediately the moment the idle session crosses the limit, without waiting for the warm edge. If both are set, `auto_compress` wins. A non-positive threshold disables both. The trigger is server-owned, so it fires even after the app detaches; no arguments echo back — an attached app sees the ordinary compress `activity`/`say`. |
| `restart`       | `{ "mode?": "rebuild" }`                  | ask the server to rebuild and/or restart itself. The server fires `SPAWNER_RESTART_CMD` (a detached command that SSHes to the host and runs `deploy/rebuild-container.sh`) and broadcasts a `say` to every client; when the container is recreated the app auto-reconnects once the fresh process is listening. `mode` selects what happens: **`build`** rebuilds the image only and leaves the running container in place — the caller's live session is **not** bounced, and the new image is staged for a later restart; **`bounce`** recreates the container from the existing image without recompiling (fast); **`rebuild`** (the default, used when `mode` is absent and by the voice command) builds then recreates. The server passes the mode to `rebuild-container.sh` by substituting the `%REBUILD%` token in `SPAWNER_RESTART_CMD` with `build`/`bounce`/`rebuild`. An unknown `mode` replies with an `error` (`restart_failed`). Any authenticated client may trigger this. If the command is unset, the server likewise replies with `restart_failed` instead of silently doing nothing. |
| `speak`         | `{ "id": "<client id>", "text": "...", "voice?": "", "format?": "" }` | ask the server to synthesize `text` (markdown already stripped client-side) via the resident Kokoro TTS server. `id` is a client-chosen correlation id echoed on the response stream; `voice` (optional) overrides the server default (`SPAWNER_TTS_VOICE`); `format` (optional) overrides the response format (`SPAWNER_TTS_FORMAT`) so each client kind pulls the encoding its playback path wants — one of `mp3` | `wav` | `opus` | `flac` | `pcm` (Android streams `pcm` straight into an AudioTrack; the browser asks for `mp3` — `decodeAudioData` wants one complete clip and mp3 decodes in every browser). The speak/mute/summary-only decision stays client-local — nothing is synthesized for a client that doesn't ask. Refused with an immediate error-bearing `speak_end` when server TTS is disabled, `text` is blank, `format` is unknown, or the connection's speak queue (32) is full — the client falls back to on-device TTS. -> `speak_audio` + binary frames + `speak_end` |
| `speak_stop`    | `{}`                                      | barge-in for server TTS: drop every queued `speak` on this connection and abort the in-flight synthesis (its stream closes with a `speak_end` `error` of `cancelled`; dropped queued requests get no `speak_end` — the client that barged in already forgot them). Sent alongside halting local playback when the user stops speech. No-op when nothing is queued or playing. |
| `tts_voices`    | `{}`                                      | ask for Kokoro's voice catalogue (relayed live from `/v1/audio/voices`) for the audio-settings voice picker -> `tts_voices` |
| `commit`        | `{}`                                      | force-commit the hands-free buffer (used by the client-side silence timeout); no-op if the buffer is empty |
| `discard_draft` | `{}`                                      | drop the uncommitted hands-free draft (buffer + audio) without committing it, and clear the on-screen draft (`pending ""`); sent when hands-free is toggled off mid-draft so a stale draft can't bleed into the next capture |
| `hosts`         | `{}`                                      | request the app-managed SSH host registry (Settings → Hosts) -> `host_list` |
| `host_put`      | `{ "host": { "name": "<name>", "address": "<host/ip>", "user?": "", "port?": 22, "key_file?": "", "identity?": "", "claude_bin?": "" } }` | add or update (upsert by `name`) an SSH host used for SSH-native execution. The app is the source of truth; the server persists it (survives restarts, shared across clients). `address` is dialed literally (NOT an `~/.ssh/config` alias). `identity` names a managed keypair (see `identities`) and, when set, supersedes `key_file`. **Trust-on-first-use:** the server scans the host's SSH key and records it in the server's known_hosts (so the first turn/browse isn't refused) — idempotent, so re-saving is a no-op. Errors `bad_host` if `name` is empty, or (after saving) if the host couldn't be reached to record its key. -> `host_list` broadcast to every client |
| `host_delete`   | `{ "name": "<name>" }`                    | remove an SSH host from the registry by name; also **forgets** its known_hosts record -> `host_list` broadcast to every client |
| `identities`    | `{}`                                      | request the app-managed SSH identity registry (Settings → Identities) -> `identity_list` |
| `identity_create` | `{ "name": "<name>", "user": "<login user>", "password?": "", "gen_key?": true }` | register an identity for `user` (required — the default SSH login user a host can override). `gen_key` (default true) generates a fresh keypair; `password` (optional) adds SSH password auth. A key-less identity must have a password. The server keeps the private key and password; only the public key is returned. Errors `bad_identity` on empty name/user or a duplicate. -> `identity_list` broadcast to every client |
| `identity_import` | `{ "name": "<name>", "user": "<login user>", "password?": "", "key_path": "<server path>" }` | register an existing server-side private key (e.g. the config default key) as a managed identity for `user`: copies it into the keys dir and records its public key. Errors `bad_identity` on empty name/user/path or an unreadable/encrypted key. -> `identity_list` broadcast to every client |
| `identity_update` | `{ "name": "<name>", "user": "<login user>", "set_password?": false, "password?": "" }` | update an existing identity's `user`, keeping its keypair. When `set_password` is true the `password` is applied (empty clears it); otherwise the current password is kept. Errors `bad_identity` on empty name/user, unknown identity, or leaving a key-less identity with no password. -> `identity_list` broadcast to every client |
| `identity_delete` | `{ "name": "<name>" }`                  | remove an identity and its private key by name -> `identity_list` broadcast to every client |
| `profile_put`   | `{ "profile_def": { "name": "<name>", "target?": "host\|sandbox", "default?": false, "image?": "", "home_mount?": "", "mounts?": [], "creds?": [], "env?": {}, "run_args?": [], "vars?": {} } }` | add or update (upsert by `name`) an execution profile. The app is the source of truth; the server persists the catalogue (survives restarts, shared across clients). String fields are `{{.Var}}`-templated per turn. Errors `bad_profile` on an empty name or an invalid env key. -> `profiles` broadcast to every client |
| `profile_delete` | `{ "name": "<name>" }`                   | remove an execution profile by name -> `profiles` broadcast to every client |
| `profile_set_default` | `{ "name": "<name>" }`              | mark the named profile as the default (clearing the marker on all others). Errors `bad_profile` if the name is unknown. -> `profiles` broadcast to every client |
| `provider_put`  | `{ "agent": "<id>", "default_model?": "<alias>", "voice_models?": ["<alias>", …] }` | set an AI backend's app-managed overrides (Settings → Providers): `default_model` is the model alias a fresh spawn stamps (`""` = the backend's compiled default); `voice_models` is the exact set of models the voice `list models`/`use model N` commands enumerate (omit to leave it at "all"). The backends themselves are compile-time; only these overrides are stored (survives restarts, shared across clients). Every alias must name a real model of that backend. Errors `bad_provider` on an unknown backend or model. -> `agents` broadcast to every client |
| `ping`          | `{}`                                      | keepalive                                     |

Audio framing: client sends `wake` (with a `codec`, optional `hands_free` / `calibrate`, and optional
`session_id`),
then binary audio, then `audio_end`. `calibrate: true` is a one-shot end-token calibration probe:
the clip is transcribed with the fast/tiny model and returned as a `calibration` message (so the
user can hear how their chosen end token is being heard) instead of being dictated. The server assembles the bytes, decodes to WAV, transcribes (whisper.cpp), then:

```
codec = "ogg_opus"   (what the Android app sends; Ogg/Opus, ~24 kbps mono 16 kHz)
codec = "pcm16"      (raw PCM16LE / 16 kHz / mono — server wraps in a WAV header;
                      what the web client sends, and what an omitted codec means)
any other codec      → rejected with a `bad_message` error; no capture starts
session_id set       → server validates that session and makes this connection follow it before
                       routing dictation/commands; empty preserves the older attached-connection target
hands_free = false   → immediate: emit `transcript`, dispatch as a typed `utterance`
hands_free = true    → streaming: APPEND the transcript to the per-connection message buffer
                       (shown live as a `pending` draft); nothing is sent to Claude until the
                       end token (`hello.end_token`, default "beep") is spoken. On the end token
                       the message commits: "hey buddy" anywhere splits out a command (processed
                       first; "cancel message" discards the buffer), the rest is dictated.
```

`hello` also carries optional flags: `end_token` (the word that commits a hands-free message),
`wake_token` (custom wake word(s), **comma-separated** for several misheard variants, accepted
alongside the built-in "hey buddy"; empty = built-in only), `speak_token` + `dictation_gate` (the
**dictation gate**: when `dictation_gate` is true and `speak_token` is set, hands-free speech is
only dictated to Claude when it follows the speak token — a comma-separated start marker — up to the
end token; un-bracketed speech is discarded, so ambient chatter/radio never reaches the session.
Commands ("hey buddy …") are never gated. Empty `speak_token`, or `dictation_gate` false, disables
the gate), `stt_mode`/`stt_model`/`whisper_url`/`whisper_model` (transcription), `wake_service` (which
backend scores the live wake/end tokens: `whisper` — the default — string-matches the fast transcript,
which is always available; `detector` opts this client into the purpose-trained `SPAWNER_WAKEWORD_URL`
sidecar. The detector is opt-in per client, so a server with the sidecar configured never routes a
default/older client through it; when `detector` is chosen but no sidecar is configured or it errors,
detection falls back to the Whisper string-match), `aliases` (misheard→command
fixups), `brief` (append a "reply briefly for TTS" hint to dictation), `interactive` (let Claude
ask clarifying questions mid-task, delivered as `ask`), and `warm_compress`/`auto_compress`/`auto_compress_threshold`
(the initial value of the server-global context-compression preference — see the `auto_compress` message for
its semantics; the app also pushes changes live with that message). Interactive mode appends its instruction to
only the **first** turn of a context — Claude retains it via `--resume`, so re-sending it every turn
would just burn tokens; a `clear` (context rotation) re-primes it. The server sends
`pending {text}` as the buffer grows (empty `text` clears the draft). The app may also send
`{"type":"commit"}` to force-commit the buffer (used by the optional client-side silence timeout);
it's a no-op if the buffer is empty.

The app records **Ogg/Opus** (MediaRecorder) and sends the whole encoded clip on release — ~10×
smaller than raw PCM (a 10 s clip is ~25 KB vs ~320 KB), which matters on capped cellular. The
server decodes Opus → 16 kHz mono WAV with **ffmpeg** (whisper can't read Opus directly) before
running whisper. `pcm16` remains supported for clients that stream raw frames. An utterance is
capped at ~120 s.

## Server -> app

| type            | payload                                              | meaning                                  |
|-----------------|------------------------------------------------------|------------------------------------------|
| `hello_ok`      | `server_version`, `session_id`, `whisper_model`, `whisper_model_fast`, `whisper_models`, `whisper_models_local`, `tts` | auth accepted; `tts` = the server offers Kokoro speech synthesis (`SPAWNER_TTS_URL` set), so the client may send `speak` instead of using on-device TTS; `whisper_model` / `whisper_model_fast` = the accurate and fast resident whisper models (`whisper_model_fast` empty when no fast server is configured); `whisper_models` = the curated English-model catalogue offered as a picker (plus any extra ggml file on disk), and `whisper_models_local` = the subset already downloaded — an entry in `whisper_models` but not `whisper_models_local` is fetched on select. Both empty when `SPAWNER_WHISPER_MODELS_DIR` isn't set (free-text entry) |
| `agents`        | `{ "agents": [{ "id", "name", "default_model", "models": [{ "alias", "voice" }] }], "default": "claude" }` | the AI backend registry, pushed once right after `hello_ok` and re-broadcast after any `provider_put`: each backend's id, display name, *effective* default model alias (the Providers-tab override, else the compiled default), and selectable models. Each model carries `voice` — whether the voice `list models`/`use model N` commands enumerate it (toggled per model in the Providers tab). `default` is the backend a spawn gets when none is chosen. The app uses it to offer a backend + model choice in the new-session picker (sent in `spawn_at`), to label the model catalogue, and to render Settings → Providers. |
| `profiles`      | `{ "profiles": [{ "name", "target", "default", "image", "home_mount", "mounts", "creds", "env", "run_args", "vars" }], "default": "<name>" }` | the app-managed execution-profile catalogue (Settings → Profiles), pushed once right after `agents` and broadcast after any `profile_put`/`profile_delete`/`profile_set_default`. Each entry is the full profile so the editor can round-trip it; omitted fields are empty. `default` (top level) names the marked-default profile a no-choice session resolves to (empty catalogue → `""`); a profile's own `default` flag marks it. Default-first order. Older clients read only `name`/`target`. |
| `whisper_model` | `{ "model": "<name>", "fast_model": "<name>", "whisper_models": ["<name>"], "whisper_models_local": ["<name>"] }` | a server-global whisper model changed: the accurate and fast servers' current models (`fast_model` empty = no fast server), plus the picker catalogue and the downloaded subset as in `hello_ok`. Broadcast to all clients; response to `set_whisper_model`. |
| `whisper_download` | `{ "model": "<name>", "fast": false, "received": 123, "total": 456, "done": false, "error": "" }` | progress of an on-demand ggml download (the server fetches a catalogue model that isn't on disk when a client selects it). `received`/`total` are bytes (`total` 0 = unknown); `done` marks completion; `error` is set with `done` on failure; `fast` echoes which server it's for. Broadcast to all clients. |
| `transcript`    | `{ "text": "...", "final": true }`                   | STT result (may stream partials)         |
| `pending`       | `{ "text": "..." }`                                  | hands-free live draft as the message buffer grows; empty `text` clears the draft |
| `calibration`   | `{ "text": "..." }`                                  | end-token calibration probe result (response to a `wake` with `calibrate: true`); shown, not dictated |
| `activity`      | `{ "text": "🤔 thinking…" }`                         | what Claude is doing right now during a turn (thinking / running a tool / editing a file); transient status line, not spoken |
| `transcribing`  | `{}`                                                 | a committed hands-free clip is being re-transcribed accurately (between the draft clearing and the `transcript`); the app shows "transcribing…" instead of snapping back to "listening". Superseded by the `transcript` that follows (or a `pending` clear if nothing was recognized) |
| `files`         | `{ "files": ["a.go", "b.md"] }`                      | basenames of files changed so far this turn; a persistent "edited: …" chip |
| `stop_speaking` | `{}`                                                 | barge-in: the client should halt any in-progress TTS immediately (from "stop" / push-to-talk) |
| `speak_audio`   | `{ "id": "<client id>", "codec": "opus" }`           | heads one synthesized utterance (response to `speak`): the **binary frames** that follow, up to the matching `speak_end`, are its audio in `codec` (the request's `format`, defaulting to the server's `SPAWNER_TTS_FORMAT`: `opus` \| `mp3` \| `wav` \| `flac` \| `pcm`; `pcm` is raw 24 kHz 16-bit little-endian mono). This is the only server→client binary; speaks are serviced **one at a time per connection, in request order**, so the frames between a header and its end always belong to that `id` — streams never interleave |
| `speak_end`     | `{ "id": "<client id>", "error": "" }`               | closes a speak stream. `error` empty = success; non-empty (synthesis failed, tts disabled, empty text, bad format, queue full — possibly with no `speak_audio` ever sent) means the client should fall back to on-device TTS for that utterance |
| `tts_voices`    | `{ "voices": [], "default": "af_heart", "error": "" }` | reply to `tts_voices`: the selectable Kokoro voice ids and the server-default voice (`SPAWNER_TTS_VOICE`), which the picker shows as "server default". A chosen voice is client-local — it rides each `speak` request's `voice` field, nothing is stored server-side. `error` non-empty (tts disabled, voices unavailable) = no catalogue |
| `speech_mode`   | `{ "summary_only": true }`                           | set the client's speech verbosity (from the "summary only" / "speak everything" voice commands; the app's audio settings has the same switch). When `summary_only` is true the client speaks only a turn's final result and plays a soft beep in place of reading each intermediate streamed step aloud; false speaks everything |
| `say`           | `{ "text": "ok bud, where do you want it?" }`        | app should speak this (TTS) + display    |
| `dialog`        | `{ "state": "await_dir", "prompt": "..." }`          | current dialog state (drives the UI)     |
| `session_list`  | `{ "sessions": [{ "name", "dir", "target?", "agent?", "model?", "profile?" }] }`    | response to `list`. `target` present only for non-host sessions (`"sandbox"`), so the app can badge them. `agent` is the AI backend id (`"codex"`; omitted for the default Claude / pre-registry records), `model` the session's current model alias, and `profile` the non-default execution profile name — so the app can badge which AI, model, and environment a session runs |
| `listing`       | `{ "path": "...", "parent": "...", "entries": [{ "name", "path", "repo", "dir" }] }` | directory contents on the browsed host (response to `browse`). `path` is the listed absolute dir; `parent` is the dir above for "up" navigation (`""` at the filesystem root `/`). `dir` is `true` for a subdirectory, `false` for a regular file (files appear only when `browse` set `files: true`; directories sort first) |
| `file_saved`    | `{ "path": "..." }`                                   | an `upload` landed; `path` is the file's absolute location on the target host (the app prefills the message box with it) |
| `file_data`     | `{ "name": "...", "path": "...", "content": "<base64>" }` | response to `download`: the file's base64 bytes, plus its `name` and source `path` for a "save as" default |
| `attached`      | `{ "name": "claude-xyz", "session_id": "<uuid>", "agent"?: "codex", "model"?: "gpt-5.5", "profile"?: "open", "usage"?: {…}, "usage_at"?: 1720099200 }` | now in passthrough mode. `agent`/`model` (omitted for the default Claude / pre-registry records) name the session's AI backend and current model so the app can show them in the status bar. `profile` names the non-default execution profile selected for the session. `session_id` is the session's stable on-disk id — the app keys the attached session by it (names diverge between servers) so the title always reflects the current server's name for it. When the session already has an on-disk transcript, `usage` (same `{ "input", "output", "cache_write", "cache_read" }` shape as `output`) carries the **last turn's context size** — read from the transcript, not a live turn — so the app shows the context meter (and how much a `clear`/`compress` would reclaim) immediately on attach. `usage_at` is that turn's unix seconds, anchoring the cache-warm countdown to its real age. Both are omitted for a freshly spawned session that hasn't run a turn. |
| `detached`      | `{}`                                                  | left passthrough mode                    |
| `context_reset` | `{ "name": "...", "session_id": "..." }`              | the session's Claude context was rotated to a fresh one — a `clear` (empty) or a `compress` (seeded with a summary). The app drops its last-turn token accounting so the status-bar context-size readout returns to zero; nothing shows again until the next dictation runs against the new context (which reports the true new size). The rotation mints a fresh `session_id` (the old one retired onto the session's prior-id chain), carried here so the app resets that session's locally-cached message rows and re-requests fresh history rather than inferring the change. Emitted for `clear`/`compress` and their voice commands. |
| `renamed`       | `{ "old": "...", "name": "...", "session_id": "<uuid>" }` | the currently-attached session was renamed (from the sidebar `rename_discovered` or the `rename` command). Carries the `old` and new `name` plus the stable `session_id` — the app matches by id (names diverge between servers) and updates the attached-session **title in place** — no history refetch or context-meter reseed (unlike a fresh `attached`). Sent only to the connection whose attached session was the one renamed; other clients pick up the new name via the refreshed `discovered`/session list. |
| `output`        | `{ "name": "...", "text": "...", "chunk": true }`     | clean session output (for display + TTS). Claude's prose **streams live**: one `chunk: true` message per assistant text message as it lands, then a final `chunk: false` closing the turn. A client that saw the stream shows/speaks the chunks and treats the final as an end marker; a client that missed it (a reply buffered because nobody was attached, or because the client was briefly unreachable — backgrounded / a mobile stall — so the send failed) gets only the final `chunk: false` and renders that. Such a buffered-undelivered reply is **not** discarded by the next turn: the server redelivers it the moment the socket accepts a write again (at the next turn's start) or when the client reattaches, so a reply that landed during a stall still reaches the app. The final `chunk: false` message also carries a `usage` object — `{ "input", "output", "cache_write", "cache_read" }` token counts for the turn (from the stream-json `result` event's aggregate `usage`) — and `usage_at`, this turn's completion time in unix seconds. `cache_read > 0` means a warm prompt-cache hit; `cache_write > 0` means the cache was (re)built. The app renders this as a per-message token badge and drives its cache-warm indicator, anchoring the cache-warm countdown to `usage_at` so a reply delivered buffered on reconnect still counts down from the turn's real age rather than from when it arrived. Streaming `chunk: true` messages omit both (no per-chunk accounting). |
| `history`       | `{ "name": "...", "messages": [{ "index", "role": "user"\|"claude", "text", "ts": <unix s>, "usage"?: {…} }], "more": true, "count": <int>, "hash": "<hex>", "unchanged": false }` | a page of past conversation (oldest→newest); `more` = older messages remain to page in. `ts` is the message's transcript timestamp in unix seconds (0 if the transcript line lacked one). A `claude` message carries `usage` (same `{ "input", "output", "cache_write", "cache_read" }` shape as `output`) so the per-message token badge survives a reattach or server restart — read from the transcript and set only on the **final** assistant line of a turn (matching the live badge, which lands on the closing message); omitted otherwise. `user` messages have the server-injected prompt scaffolding (the brief-reply nudge, interactive-mode ask instruction, compress recap preamble) stripped, so replayed history matches the live echo exactly and the app can dedupe a turn against its live copy. `count`/`hash` are the whole chain's digest — the app stores them with its cached transcript and later sends `hash` back as `have_hash` to check freshness. `unchanged: true` answers a top-page request whose `have_hash` still matched: `messages` is empty and the app keeps its cache untouched. Response to `history`. |
| `digests`       | `{ "items": [{ "name", "session_id", "count": <int>, "hash": "<hex>" }] }` | one transcript digest per registered session (response to `digest`, sent on connect). `count` is the message total and `hash` an opaque content hash of the whole chain; the app compares each against the digest stored with its offline cache to know which sessions changed while it was away — and refetches history only for those, keeping every unchanged session body-free. |
| `read_last`     | `{ "count": <int> }`                                 | app re-reads (TTS) + scrolls to the last `count` Claude replies in the current session (from the `read last X` command) |
| `discovered`    | `{ "sessions": [{ "name", "dir", "session_id", "last_active": <unix s>, "active": <bool>, "registered": <bool>, "busy": <bool>, "target?", "host?", "agent?", "model?", "profile?" }] }` | the session list: **every registered session as its own row** (keyed by its `session_id`, so multiple sessions in one directory are each shown and separately manageable), plus one adoptable row per **unregistered** directory found on disk. `active` = an interactive `claude` is open in tmux at that dir (driving it then risks a two-writer conflict); `registered` = already in the store; `busy` = a dictation turn is running for it now; `target` present only for non-host sessions (`"sandbox"`), so the app can badge them. `host` present only for sessions running on a registered SSH host (its host name; omitted = local), so the app can group them. `agent`/`model` (registered rows only; `agent` omitted for the default Claude) name the AI backend + current model, and `profile` names the non-default execution profile, so the app can badge them. Response to `discover`. |
| `rate_limit`    | `{ "status": "allowed", "resets_at": <unix s>, "limit_type": "five_hour", "using_overage": false }` | the Claude subscription's usage-window state, from the stream-json `rate_limit_event` (emitted early in every turn). `limit_type` names the binding window (`five_hour` = the rolling session window, or the weekly cap); `resets_at` is when it resets; `status` is a **coarse** signal (`allowed` until the cap nears — Anthropic exposes no exact remaining quota); `using_overage` = drawing on pay-as-you-go overage. Shown as the session-limit readout at the bottom of the sessions drawer; not spoken. Emitted on each turn, and also **once right after `hello_ok`** (the server caches the last value) so a freshly-connected app shows the limit without waiting for a turn — the cache is empty until the first turn after a server restart. |
| `usage`         | `{ "session_pct": 40, "session_reset": "Jul 4, 9:59am", "week_pct": 41, "week_reset": "Jul 4, 5:59pm", "text": "<full /usage report>" }` | the Claude plan's usage report (response to a `usage` request or the "usage" voice command). `session_pct` / `week_pct` are percent-used parsed from `/usage` (**-1** when unparseable); `text` is the full report shown verbatim (session/weekly headline + local contributing breakdown). The app shows it in a usage sheet. |
| `usage_estimate`| `{ "calibrated": true, "session_est_pct": 52.3, "week_est_pct": 43.1, "session_real_pct": 40, "week_real_pct": 41, "cum_tokens": 12345678, "tokens_since_check": 456789, "turns_since_check": 14, "last_check_at": <unix s>, "bench_set": true, "bench_sess_pct": 40, "bench_week_pct": 41, "bench_tokens": 12000000, "tokens_since_set": 345678 }` | the server-global **drift-live usage estimate**, aggregated across ALL sessions and clients. `*_est_pct` drift up every turn (from summed weighted token cost, using a tokens-per-percent rate learned from successive `/usage` calibrations); `*_real_pct` are the last `/usage` calibration's true numbers. `-1` on the `*_pct` fields (or `calibrated: false`) means no `/usage` anchor yet, so no estimate. The `bench_*` fields describe an armed manual benchmark (`usage_set`): the percentages/odometer it was stamped at and `tokens_since_set` burned since, which `usage_calc` divides by the percent gained to set the rate directly. Emitted after **every turn** (drift), after a `/usage` calibration (snap to real), and pushed once on connect. The app shows the estimate in the drawer footer + usage sheet. |
| `host_list`     | `{ "hosts": [{ "name", "address", "user?", "port?", "key_file?", "identity?", "claude_bin?" }] }` | the app-managed SSH host registry (Settings → Hosts). Response to `hosts`, and broadcast to every client after a `host_put`/`host_delete` so the shared list stays in sync. |
| `identity_list` | `{ "identities": [{ "name", "user", "public_key", "has_password" }] }` | the app-managed SSH identity registry (Settings → Identities) — name, default login `user`, PUBLIC key (empty for a password-only identity), and whether a password is set. The private key and the password itself never leave the server. Response to `identities`, and broadcast after `identity_create`/`identity_import`/`identity_delete`. |
| `error`         | `{ "code": "...", "message": "..." }`                 | spoken/displayed error feedback          |
| `turn_interrupted` | `{ "name": "...", "reason": "server restarting" }` | an in-flight dictation turn was abandoned server-side (turns don't survive a server restart). The app clears its "thinking…" state and prompts the user to resend, instead of waiting on a reply that will never arrive. |
| `turn_stopped`  | `{ "name": "..." }`                                   | a running turn was deliberately aborted (the `abort` message / "stop the turn" command). The app clears its "thinking…" state without the "say it again" nudge. |
| `diff`          | `{ "text": "..." }`                                   | a compact `git diff --stat` review summary after a turn that changed files; shown as a note, not spoken. |
| `ask`           | `{ "name": "...", "questions": [{ "q": "...", "options": ["...", ...] }] }` | interactive mode: Claude needs clarification. The app renders the questions (chips for `options`, text fields otherwise) and reads them aloud; answers go back as an ordinary dictation turn. `options` omitted = free-text. |
| `pong`          | `{}`                                                  | keepalive reply                          |

## Output path note

`output` messages carry **clean text only** — headless `stream-json` already yields clean prose,
no ANSI/TUI scraping (see the TUI-capture decision in `CLAUDE.md`). The app feeds `output.text`
straight to TTS.

Claude's reply **streams**: the server emits one `chunk: true` message per assistant text message
as `claude` produces it (whole messages, not token deltas — TTS wants whole sentences), then a
single `chunk: false` message closes the turn. The closing message's `text` is the final `result`
(the last assistant message). Clients dedupe by tracking whether they saw any `chunk: true` for the
current turn: if so, the chunks were already shown/spoken and the `chunk: false` is just an
end-of-turn marker; if not (the turn finished while the client was detached and its reply was
buffered for reconnect), the client renders/speaks the `chunk: false` text. The interactive-mode
ASK block is never streamed — it's withheld and delivered as a structured `ask` at turn end.

## Example: spawn dialog over the wire

```jsonc
// user said "hey buddy, spawn a new session"
app -> { "type": "wake" }
app -> <binary audio...>
app -> { "type": "audio_end" }
srv -> { "type": "transcript", "text": "spawn a new session", "final": true }
srv -> { "type": "dialog", "state": "await_dir", "prompt": "ok bud, where do you want it?" }
srv -> { "type": "say", "text": "ok bud, where do you want it?" }

app -> { "type": "wake" }
app -> <binary audio...>
app -> { "type": "audio_end" }
srv -> { "type": "transcript", "text": "in data claude underscore claude", "final": true }
// only when a sandbox image is configured — otherwise the flow skips straight to await_attach (host):
srv -> { "type": "dialog", "state": "await_target", "prompt": "host or sandbox?" }
srv -> { "type": "say", "text": "run claude-claude on the host, or in a sandbox?" }
// ... user answers "host" ...
srv -> { "type": "say", "text": "ok, made that directory. want to attach?" }
srv -> { "type": "dialog", "state": "await_attach", "prompt": "want to attach?" }

app -> { "type": "wake" }
app -> <binary audio...>
app -> { "type": "audio_end" }
srv -> { "type": "transcript", "text": "yes", "final": true }
srv -> { "type": "attached", "name": "claude-claude" }
```

## Error codes

Every failure sends an `error` message (machine-readable, always displayed). For the codes a
**voice** user can actually trigger, the server *also* sends a friendly spoken `say` alongside it
(e.g. `bad_path` → "that path won't work, bud…"), so a spoken command never fails silently. The
wire-level / programmer-facing codes that only come from the app — `bad_message`, `bad_adopt`,
`bad_delete`, `bad_rename`, `bad_host`, `bad_identity`, `bad_profile`, `bad_provider`, `unauthorized`, `internal` — stay screen-only (no spoken line).

| code               | meaning                                                  |
|--------------------|----------------------------------------------------------|
| `unauthorized`     | bad/missing token                                        |
| `bad_message`      | malformed/unparseable client message                     |
| `bad_path`         | spawn path is not an absolute existing directory (or a sandbox target with no sandbox configured) |
| `bad_adopt`        | invalid `adopt` request                                  |
| `bad_delete`       | invalid `delete`/`delete_discovered` request             |
| `bad_rename`       | invalid `rename`/`rename_discovered` request             |
| `bad_host`         | invalid `host_put`/`host_delete` request (missing name)  |
| `bad_identity`     | invalid `identity_create`/`identity_delete` (missing/duplicate name) |
| `bad_profile`      | invalid `profile_put`/`profile_delete`/`profile_set_default` (missing name, bad env key, or unknown profile) |
| `bad_provider`     | invalid `provider_put` (missing/unknown backend, or a default/voice alias that isn't a model of that backend) |
| `bad_agent`        | invalid `set_agent` request (needs `session_id` or `path`) |
| `spawn_failed`     | session directory creation / claude failed to start      |
| `no_session`       | action referenced an unknown session                     |
| `not_found`        | referenced directory/session not found                   |
| `not_implemented`  | audio path invoked but STT is disabled (no whisper)      |
| `file_too_large`   | `upload`/`download` file exceeds the 64 MiB transfer cap |
| `session_active`   | refused: an interactive `claude` is live in a terminal   |
| `discover_failed`  | scanning `~/.claude/projects` failed                     |
| `history_failed`   | reading a session transcript failed                      |
| `rename_failed`    | rename could not be persisted                            |
| `transcribe_failed`/`whisper_failed` | STT engine error                       |
| `turn_failed`      | the dictation turn errored (non-success `result`)        |
| `compress_failed`  | the `compress` summarization turn errored                |
| `usage_failed`     | running `/usage` to fetch the plan's usage report failed |
| `restart_failed`   | `restart` requested but `SPAWNER_RESTART_CMD` is unset/failed to launch |
| `internal`         | unexpected server error                                  |

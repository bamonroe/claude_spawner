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
| `hello`         | `token`, `app_version`                   | auth handshake                                |
| `wake`          | `{}`                                      | wake word fired; audio frames will follow     |
| *(binary)*      | raw PCM/Opus frames                       | audio chunk (between `wake` and `audio_end`)  |
| `audio_end`     | `{}`                                      | end of utterance; server finalizes transcript |
| `utterance`     | `{ "text": "<what the user said>" }`      | **the text seam** — a complete utterance as text (post-STT or typed). Implemented today; the audio path above produces one of these server-side once Whisper lands. |
| `reply`         | `{ "text": "<user reply>" }`              | alias of `utterance` for dialog replies       |
| `attach`        | `{ "session_id"?: "<uuid>", "name"?: "<session>", "silent": false }`| request attach. Prefer `session_id` (the stable handle — survives renames and is the same session across servers); the server resolves it to the current name, falling back to `name` if the id is unknown or absent. `silent: true` suppresses the spoken "attached… go ahead, bud." confirmation (used for the app's auto re-attach on reconnect); a finished turn's buffered result is still delivered. |
| `detach`        | `{}`                                      | leave passthrough                             |
| `list_sessions` | `{}`                                      | request the session list (quiet; for the sidebar) -> `session_list` |
| `discover`      | `{}`                                      | scan `~/.claude/projects` for ALL Claude sessions (spawner-created or not, e.g. interactive `claude` in tmux) -> `discovered` |
| `adopt`         | `{ "session_id": "<uuid>", "path": "<dir>" }` | register a discovered session into the store and attach to it (so the app can view/drive it via `--resume`) -> `attached` + `session_list` |
| `delete_discovered` | `{ "session_id": "<uuid>" }`          | PERMANENTLY delete ONE session — its transcript(s) (the current `session_id` plus any rotated prior ids) and its single registry record — targeted by id, so its dir-mates are left intact. Refused with `session_active` if the directory is live in a terminal. -> refreshed `discovered` + `session_list` |
| `rename_discovered` | `{ "session_id": "<uuid>", "path": "<dir>", "new_name": "<name>" }` | give a discovered session a custom name, resolving the target by `session_id` (registers it — by `path` if not yet in the store — without attaching). -> refreshed `discovered` + `session_list` |
| `rename`        | `{ "name": "<old>", "new_name": "<new>" }`| rename a session (keeps its session_id) -> `session_list` |
| `delete`        | `{ "name": "<session>" }`                 | delete a session record -> `session_list`     |
| `browse`        | `{ "path": "<dir or empty>", "host_name": "", "files": false }` | list a directory **on `host_name`** (over SSH; `""` = local/loopback) for the New-session picker. Empty `path` = that host's filesystem root `/`. No spawn-root jail here — the picker walks the whole host. `files` optional (default `false`): when `true` the listing also includes regular files (each entry's `dir` flag distinguishes them) for the file-transfer picker; otherwise only subdirectories are returned. -> `listing` |
| `upload`        | `{ "path": "<dest dir>", "name": "<filename>", "host_name": "", "content": "<base64>" }` | write an uploaded file to `name` inside directory `path` **on `host_name`** (over SSH; `""` = local/loopback). `content` is the file's bytes, base64-encoded, in one message (capped at 64 MiB → `file_too_large`). `name` is reduced to its basename so it can't escape `path`. -> `file_saved` |
| `download`      | `{ "path": "<file path>", "host_name": "" }` | read the file at absolute `path` **on `host_name`** (over SSH; `""` = local/loopback) and return its bytes (capped at 64 MiB → `file_too_large`). -> `file_data` |
| `spawn_at`      | `{ "path": "<dir>", "target": "host\|sandbox", "create": false, "host_name": "", "agent": "", "model": "" }` | open `path` **on `host_name`**: if it already has a registered session on that host, attach to that one (no duplicate `-2` is minted); otherwise create a session there and attach -> `attached` (+ `session_list` when a new one was created). `path` must be absolute. `target` optional (default `host`); `sandbox` runs turns in an isolated container and errors if no sandbox image is configured. `create` optional (default `false`): when `true`, `mkdir` the `path` on the target host first so the picker can start a project in a directory that doesn't exist yet — errors `bad_path` if the folder already exists. `host_name` optional (default `""` = local): the registered SSH host (Settings → Hosts) to browse / run the session on; ignored for `sandbox`. `agent` optional (default `""` = default backend): the AI backend id (from the `agents` message) the session runs. `model` optional (default `""` = the backend's default): a model alias from that backend; an unknown model falls back to the default |
| `history`       | `{ "name": "<session>", "before": <int?>, "limit": <int> }` | request a page of that session's past conversation (from Claude's transcript). `before` = exclusive index cursor (omit for the most recent page; page older by passing the oldest index held). Spans context rotations: after a `clear`, the retired transcripts and the current one are stitched into one continuous, contiguously-indexed conversation. -> `history` |
| `clear`         | `{}`                                      | rotate the attached session's Claude context: retire the current `session_id` (its transcript kept for `history`) and start a fresh one, so the next dictation replays no prior context. No model tokens spent; history still spans the whole chain. -> `context_reset` + `say` |
| `compress`      | `{}`                                      | compact the attached session's Claude context: run a background turn asking Claude to summarize the conversation, then rotate the `session_id` (old transcript kept for `history`) and carry the summary forward — it seeds the next dictation so Claude continues with the condensed context instead of dropping it (the `/compact` analogue of `clear`). Emits an `activity` breadcrumb while summarizing, then a `context_reset` (readout back to zero) and a `say`. Refused if a turn is in flight (`say`) or no turn has run yet (`say`). -> `activity` + `context_reset` + `say` |
| `cancel`        | `{}`                                      | abort current dialog                          |
| `usage`         | `{}`                                      | fetch the Claude plan's usage report by running `claude -p "/usage"` headless (a real but lightweight invocation) -> `usage`. The "usage" voice command does the same but also speaks a summary. |
| `usage_set`     | `{}`                                      | arm a manual two-point rate benchmark: read `/usage`, then stamp the current odometer position and real percentages as the start mark (the app's **"set"** button). -> `usage` + `usage_estimate` + `say`. |
| `usage_calc`    | `{}`                                      | close the manual benchmark: read `/usage`, then set each window's tokens-per-percent rate **directly** from `(tokens since the `usage_set` mark) / (percent gained)` — no EMA damping — and re-anchor the estimate (the app's **"calc"** button). A window that moved less than 1% since the mark is left unchanged. -> `usage` + `usage_estimate` + `say`. |
| `abort`         | `{}`                                      | cancel the running dictation turn on the attached session (kills the claude child) -> `turn_stopped` |
| `set_whisper_model` | `{ "whisper_model": "<name>" }`       | switch the server-global resident whisper model (fans out a `whisper_model` broadcast to every connected client) |
| `auto_compress`  | `{ "warm_compress": <bool>, "auto_compress": <bool>, "auto_compress_threshold": <int> }` | set the server-global **context-compression** preference live (the same three fields the `hello` handshake carries). Two independent triggers share one limit — once a started session's context exceeds `auto_compress_threshold` **thousand** tokens (measured as `input + cache_write + cache_read`, matching the app's context badge): **`warm_compress`** auto-runs a `compress` in the last ~15 s of that session's 5-minute warm-cache window (so the summary turn reuses the still-warm cache instead of paying a cold rebuild later); **`auto_compress`** compresses immediately the moment the idle session crosses the limit, without waiting for the warm edge. If both are set, `auto_compress` wins. A non-positive threshold disables both. The trigger is server-owned, so it fires even after the app detaches; no arguments echo back — an attached app sees the ordinary compress `activity`/`say`. |
| `restart`       | `{}`                                      | ask the server to restart itself. The server fires `SPAWNER_RESTART_CMD` (a detached command, typically `systemctl --user restart --no-block spawner-server`) which relaunches the service; it broadcasts a `say` to every client, and the app auto-reconnects once the fresh process is listening. Any authenticated client may trigger this. If the command is unset, the server replies with an `error` (`restart_failed`) instead of silently doing nothing. |
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
| `ping`          | `{}`                                      | keepalive                                     |

Audio framing: client sends `wake` (with a `codec`, and optional `hands_free` and `calibrate`),
then binary audio, then `audio_end`. `calibrate: true` is a one-shot end-token calibration probe:
the clip is transcribed with the fast/tiny model and returned as a `calibration` message (so the
user can hear how their chosen end token is being heard) instead of being dictated. The server assembles the bytes, decodes to WAV, transcribes (whisper.cpp), then:

```
codec = "ogg_opus"   (default; app records Ogg/Opus, ~24 kbps mono 16 kHz)
codec = "pcm16"      (raw PCM16LE / 16 kHz / mono — server wraps in a WAV header)
hands_free = false   → immediate: emit `transcript`, dispatch as a typed `utterance`
hands_free = true    → streaming: APPEND the transcript to the per-connection message buffer
                       (shown live as a `pending` draft); nothing is sent to Claude until the
                       end token (`hello.end_token`, default "beep") is spoken. On the end token
                       the message commits: "hey buddy" anywhere splits out a command (processed
                       first; "cancel message" discards the buffer), the rest is dictated.
```

`hello` also carries optional flags: `end_token` (the word that commits a hands-free message),
`wake_token` (a custom wake word accepted alongside the built-in "hey buddy"; empty = built-in only),
`stt_mode`/`stt_model`/`whisper_url`/`whisper_model` (transcription), `aliases` (misheard→command
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
| `hello_ok`      | `server_version`, `session_id`, `whisper_model`      | auth accepted; `whisper_model` = the resident model name |
| `agents`        | `{ "agents": [{ "id", "name", "default_model", "models": [{ "alias" }] }], "default": "claude" }` | the AI backend registry, pushed once right after `hello_ok`: each backend's id, display name, default model alias, and selectable models; `default` is the backend a spawn gets when none is chosen. The app uses it to offer a backend + model choice in the new-session picker (sent in `spawn_at`) and to label the model catalogue. |
| `whisper_model` | `{ "model": "<name>" }`                              | server-global whisper model changed (broadcast to all clients; response to `set_whisper_model`) |
| `transcript`    | `{ "text": "...", "final": true }`                   | STT result (may stream partials)         |
| `pending`       | `{ "text": "..." }`                                  | hands-free live draft as the message buffer grows; empty `text` clears the draft |
| `calibration`   | `{ "text": "..." }`                                  | end-token calibration probe result (response to a `wake` with `calibrate: true`); shown, not dictated |
| `activity`      | `{ "text": "🤔 thinking…" }`                         | what Claude is doing right now during a turn (thinking / running a tool / editing a file); transient status line, not spoken |
| `files`         | `{ "files": ["a.go", "b.md"] }`                      | basenames of files changed so far this turn; a persistent "edited: …" chip |
| `stop_speaking` | `{}`                                                 | barge-in: the client should halt any in-progress TTS immediately (from "stop" / push-to-talk) |
| `say`           | `{ "text": "ok bud, where do you want it?" }`        | app should speak this (TTS) + display    |
| `dialog`        | `{ "state": "await_dir", "prompt": "..." }`          | current dialog state (drives the UI)     |
| `session_list`  | `{ "sessions": [{ "name", "dir", "target?", "agent?", "model?" }] }`    | response to `list`. `target` present only for non-host sessions (`"sandbox"`), so the app can badge them. `agent` is the AI backend id (`"codex"`; omitted for the default Claude / pre-registry records) and `model` the session's current model alias — so the app can badge which AI + model a session runs |
| `listing`       | `{ "path": "...", "parent": "...", "entries": [{ "name", "path", "repo", "dir" }] }` | directory contents on the browsed host (response to `browse`). `path` is the listed absolute dir; `parent` is the dir above for "up" navigation (`""` at the filesystem root `/`). `dir` is `true` for a subdirectory, `false` for a regular file (files appear only when `browse` set `files: true`; directories sort first) |
| `file_saved`    | `{ "path": "..." }`                                   | an `upload` landed; `path` is the file's absolute location on the target host (the app prefills the message box with it) |
| `file_data`     | `{ "name": "...", "path": "...", "content": "<base64>" }` | response to `download`: the file's base64 bytes, plus its `name` and source `path` for a "save as" default |
| `attached`      | `{ "name": "claude-xyz", "session_id": "<uuid>", "agent"?: "codex", "model"?: "gpt-5.5", "usage"?: {…}, "usage_at"?: 1720099200 }` | now in passthrough mode. `agent`/`model` (omitted for the default Claude / pre-registry records) name the session's AI backend and current model so the app can show them in the status bar. `session_id` is the session's stable on-disk id — the app keys the attached session by it (names diverge between servers) so the title always reflects the current server's name for it. When the session already has an on-disk transcript, `usage` (same `{ "input", "output", "cache_write", "cache_read" }` shape as `output`) carries the **last turn's context size** — read from the transcript, not a live turn — so the app shows the context meter (and how much a `clear`/`compress` would reclaim) immediately on attach. `usage_at` is that turn's unix seconds, anchoring the cache-warm countdown to its real age. Both are omitted for a freshly spawned session that hasn't run a turn. |
| `detached`      | `{}`                                                  | left passthrough mode                    |
| `context_reset` | `{ "name": "..." }`                                   | the session's Claude context was rotated to a fresh one — a `clear` (empty) or a `compress` (seeded with a summary). The app drops its last-turn token accounting so the status-bar context-size readout returns to zero; nothing shows again until the next dictation runs against the new context (which reports the true new size). Emitted for `clear`/`compress` and their voice commands. |
| `renamed`       | `{ "old": "...", "name": "...", "session_id": "<uuid>" }` | the currently-attached session was renamed (from the sidebar `rename_discovered` or the `rename` command). Carries the `old` and new `name` plus the stable `session_id` — the app matches by id (names diverge between servers) and updates the attached-session **title in place** — no history refetch or context-meter reseed (unlike a fresh `attached`). Sent only to the connection whose attached session was the one renamed; other clients pick up the new name via the refreshed `discovered`/session list. |
| `output`        | `{ "name": "...", "text": "...", "chunk": true }`     | clean session output (for display + TTS). Claude's prose **streams live**: one `chunk: true` message per assistant text message as it lands, then a final `chunk: false` closing the turn. A client that saw the stream shows/speaks the chunks and treats the final as an end marker; a client that missed it (a reply buffered while it was detached) gets only the final `chunk: false` and renders that. The final `chunk: false` message also carries a `usage` object — `{ "input", "output", "cache_write", "cache_read" }` token counts for the turn (from the stream-json `result` event's aggregate `usage`). `cache_read > 0` means a warm prompt-cache hit; `cache_write > 0` means the cache was (re)built. The app renders this as a per-message token badge and drives its cache-warm indicator. Streaming `chunk: true` messages omit `usage` (no per-chunk accounting). |
| `history`       | `{ "name": "...", "messages": [{ "index", "role": "user"\|"claude", "text", "ts": <unix s>, "usage"?: {…} }], "more": true }` | a page of past conversation (oldest→newest); `more` = older messages remain to page in. `ts` is the message's transcript timestamp in unix seconds (0 if the transcript line lacked one). A `claude` message carries `usage` (same `{ "input", "output", "cache_write", "cache_read" }` shape as `output`) so the per-message token badge survives a reattach or server restart — read from the transcript and set only on the **final** assistant line of a turn (matching the live badge, which lands on the closing message); omitted otherwise. `user` messages have the server-injected prompt scaffolding (the brief-reply nudge, interactive-mode ask instruction, compress recap preamble) stripped, so replayed history matches the live echo exactly and the app can dedupe a turn against its live copy. Response to `history`. |
| `read_last`     | `{ "count": <int> }`                                 | app re-reads (TTS) + scrolls to the last `count` Claude replies in the current session (from the `read last X` command) |
| `discovered`    | `{ "sessions": [{ "name", "dir", "session_id", "last_active": <unix s>, "active": <bool>, "registered": <bool>, "busy": <bool>, "target?", "agent?", "model?" }] }` | the session list: **every registered session as its own row** (keyed by its `session_id`, so multiple sessions in one directory are each shown and separately manageable), plus one adoptable row per **unregistered** directory found on disk. `active` = an interactive `claude` is open in tmux at that dir (driving it then risks a two-writer conflict); `registered` = already in the store; `busy` = a dictation turn is running for it now; `target` present only for non-host sessions (`"sandbox"`), so the app can badge them. `agent`/`model` (registered rows only; `agent` omitted for the default Claude) name the AI backend + current model so the app can badge them. Response to `discover`. |
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
`bad_delete`, `bad_rename`, `bad_host`, `bad_identity`, `unauthorized`, `internal` — stay screen-only (no spoken line).

| code               | meaning                                                  |
|--------------------|----------------------------------------------------------|
| `unauthorized`     | bad/missing token                                        |
| `bad_message`      | malformed/unparseable client message                     |
| `bad_path`         | spawn path escaped allowed root                          |
| `bad_adopt`        | invalid `adopt` request                                  |
| `bad_delete`       | invalid `delete`/`delete_discovered` request             |
| `bad_rename`       | invalid `rename`/`rename_discovered` request             |
| `bad_host`         | invalid `host_put`/`host_delete` request (missing name)  |
| `bad_identity`     | invalid `identity_create`/`identity_delete` (missing/duplicate name) |
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
| `internal`         | unexpected server error                                  |

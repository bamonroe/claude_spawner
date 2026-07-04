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
| `attach`        | `{ "name": "<session>", "silent": false }`| request attach. `silent: true` suppresses the spoken "attached… go ahead, bud." confirmation (used for the app's auto re-attach on reconnect); a finished turn's buffered result is still delivered. |
| `detach`        | `{}`                                      | leave passthrough                             |
| `list_sessions` | `{}`                                      | request the session list (quiet; for the sidebar) -> `session_list` |
| `discover`      | `{}`                                      | scan `~/.claude/projects` for ALL Claude sessions (spawner-created or not, e.g. interactive `claude` in tmux) -> `discovered` |
| `adopt`         | `{ "session_id": "<uuid>", "path": "<dir>" }` | register a discovered session into the store and attach to it (so the app can view/drive it via `--resume`) -> `attached` + `session_list` |
| `delete_discovered` | `{ "session_id": "<uuid>" }`          | PERMANENTLY delete ALL transcripts for the session's directory (and its registry records). Refused with `session_active` if live in a terminal. -> refreshed `discovered` + `session_list` |
| `rename_discovered` | `{ "session_id": "<uuid>", "path": "<dir>", "new_name": "<name>" }` | give a discovered session a custom name (registers it by dir if needed, without attaching). -> refreshed `discovered` + `session_list` |
| `rename`        | `{ "name": "<old>", "new_name": "<new>" }`| rename a session (keeps its session_id) -> `session_list` |
| `delete`        | `{ "name": "<session>" }`                 | delete a session record -> `session_list`     |
| `browse`        | `{ "path": "<dir or empty>" }`            | list a directory for the New-session picker (empty = roots) -> `listing` |
| `spawn_at`      | `{ "path": "<dir>" }`                     | create a session in `path` and attach -> `attached` + `session_list` |
| `history`       | `{ "name": "<session>", "before": <int?>, "limit": <int> }` | request a page of that session's past conversation (from Claude's transcript). `before` = exclusive index cursor (omit for the most recent page; page older by passing the oldest index held). Spans context rotations: after a `clear`, the retired transcripts and the current one are stitched into one continuous, contiguously-indexed conversation. -> `history` |
| `clear`         | `{}`                                      | rotate the attached session's Claude context: retire the current `session_id` (its transcript kept for `history`) and start a fresh one, so the next dictation replays no prior context. No model tokens spent; history still spans the whole chain. -> `say` |
| `compress`      | `{}`                                      | compact the attached session's Claude context: run a background turn asking Claude to summarize the conversation, then rotate the `session_id` (old transcript kept for `history`) and carry the summary forward — it seeds the next dictation so Claude continues with the condensed context instead of dropping it (the `/compact` analogue of `clear`). Emits an `activity` breadcrumb while summarizing, then a `say`. Refused if a turn is in flight (`say`) or no turn has run yet (`say`). -> `activity` + `say` |
| `cancel`        | `{}`                                      | abort current dialog                          |
| `usage`         | `{}`                                      | fetch the Claude plan's usage report by running `claude -p "/usage"` headless (a real but lightweight invocation) -> `usage`. The "usage" voice command does the same but also speaks a summary. |
| `usage_set`     | `{}`                                      | arm a manual two-point rate benchmark: read `/usage`, then stamp the current odometer position and real percentages as the start mark (the app's **"set"** button). -> `usage` + `usage_estimate` + `say`. |
| `usage_calc`    | `{}`                                      | close the manual benchmark: read `/usage`, then set each window's tokens-per-percent rate **directly** from `(tokens since the `usage_set` mark) / (percent gained)` — no EMA damping — and re-anchor the estimate (the app's **"calc"** button). A window that moved less than 1% since the mark is left unchanged. -> `usage` + `usage_estimate` + `say`. |
| `abort`         | `{}`                                      | cancel the running dictation turn on the attached session (kills the claude child) -> `turn_stopped` |
| `set_whisper_model` | `{ "whisper_model": "<name>" }`       | switch the server-global resident whisper model (fans out a `whisper_model` broadcast to every connected client) |
| `restart`       | `{}`                                      | ask the server to restart: it broadcasts a `say` to every client, then exits non-zero so its supervisor (the systemd unit, whose `ExecStartPre` rebuilds) relaunches it on current code. Any authenticated client may trigger this; the app auto-reconnects once the fresh process is listening. Under `docker`/`go run` (no supervisor) the process just exits. |
| `commit`        | `{}`                                      | force-commit the hands-free buffer (used by the client-side silence timeout); no-op if the buffer is empty |
| `discard_draft` | `{}`                                      | drop the uncommitted hands-free draft (buffer + audio) without committing it, and clear the on-screen draft (`pending ""`); sent when hands-free is toggled off mid-draft so a stale draft can't bleed into the next capture |
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
`stt_mode`/`stt_model`/`whisper_url`/`whisper_model` (transcription), `aliases` (misheard→command
fixups), `brief` (append a "reply briefly for TTS" hint to dictation), and `interactive` (let Claude
ask clarifying questions mid-task, delivered as `ask`). Interactive mode appends its instruction to
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
| `whisper_model` | `{ "model": "<name>" }`                              | server-global whisper model changed (broadcast to all clients; response to `set_whisper_model`) |
| `transcript`    | `{ "text": "...", "final": true }`                   | STT result (may stream partials)         |
| `pending`       | `{ "text": "..." }`                                  | hands-free live draft as the message buffer grows; empty `text` clears the draft |
| `calibration`   | `{ "text": "..." }`                                  | end-token calibration probe result (response to a `wake` with `calibrate: true`); shown, not dictated |
| `activity`      | `{ "text": "🤔 thinking…" }`                         | what Claude is doing right now during a turn (thinking / running a tool / editing a file); transient status line, not spoken |
| `files`         | `{ "files": ["a.go", "b.md"] }`                      | basenames of files changed so far this turn; a persistent "edited: …" chip |
| `stop_speaking` | `{}`                                                 | barge-in: the client should halt any in-progress TTS immediately (from "stop" / push-to-talk) |
| `say`           | `{ "text": "ok bud, where do you want it?" }`        | app should speak this (TTS) + display    |
| `dialog`        | `{ "state": "await_dir", "prompt": "..." }`          | current dialog state (drives the UI)     |
| `session_list`  | `{ "sessions": [{ "name", "dir", "attached" }] }`    | response to `list`                       |
| `listing`       | `{ "path": "...", "parent": "...", "entries": [{ "name", "dir" }] }` | directory contents for the New-session picker (response to `browse`; empty `path` = the configured roots) |
| `attached`      | `{ "name": "claude-xyz" }`                            | now in passthrough mode                  |
| `detached`      | `{}`                                                  | left passthrough mode                    |
| `output`        | `{ "name": "...", "text": "...", "chunk": true }`     | clean session output (for display + TTS). Claude's prose **streams live**: one `chunk: true` message per assistant text message as it lands, then a final `chunk: false` closing the turn. A client that saw the stream shows/speaks the chunks and treats the final as an end marker; a client that missed it (a reply buffered while it was detached) gets only the final `chunk: false` and renders that. The final `chunk: false` message also carries a `usage` object — `{ "input", "output", "cache_write", "cache_read" }` token counts for the turn (from the stream-json `result` event's aggregate `usage`). `cache_read > 0` means a warm prompt-cache hit; `cache_write > 0` means the cache was (re)built. The app renders this as a per-message token badge and drives its cache-warm indicator. Streaming `chunk: true` messages omit `usage` (no per-chunk accounting). |
| `history`       | `{ "name": "...", "messages": [{ "index", "role": "user"\|"claude", "text", "ts": <unix s> }], "more": true }` | a page of past conversation (oldest→newest); `more` = older messages remain to page in. `ts` is the message's transcript timestamp in unix seconds (0 if the transcript line lacked one). Response to `history`. |
| `read_last`     | `{ "count": <int> }`                                 | app re-reads (TTS) + scrolls to the last `count` Claude replies in the current session (from the `read last X` command) |
| `discovered`    | `{ "sessions": [{ "name", "dir", "session_id", "last_active": <unix s>, "active": <bool>, "registered": <bool>, "busy": <bool> }] }` | all Claude sessions found on disk (one per dir, newest first). `active` = an interactive `claude` is open in tmux at that dir (driving it then risks a two-writer conflict); `registered` = already in the store; `busy` = a dictation turn is running for it now. Response to `discover`. |
| `rate_limit`    | `{ "status": "allowed", "resets_at": <unix s>, "limit_type": "five_hour", "using_overage": false }` | the Claude subscription's usage-window state, from the stream-json `rate_limit_event` (emitted early in every turn). `limit_type` names the binding window (`five_hour` = the rolling session window, or the weekly cap); `resets_at` is when it resets; `status` is a **coarse** signal (`allowed` until the cap nears — Anthropic exposes no exact remaining quota); `using_overage` = drawing on pay-as-you-go overage. Shown as the session-limit readout at the bottom of the sessions drawer; not spoken. Emitted on each turn, and also **once right after `hello_ok`** (the server caches the last value) so a freshly-connected app shows the limit without waiting for a turn — the cache is empty until the first turn after a server restart. |
| `usage`         | `{ "session_pct": 40, "session_reset": "Jul 4, 9:59am", "week_pct": 41, "week_reset": "Jul 4, 5:59pm", "text": "<full /usage report>" }` | the Claude plan's usage report (response to a `usage` request or the "usage" voice command). `session_pct` / `week_pct` are percent-used parsed from `/usage` (**-1** when unparseable); `text` is the full report shown verbatim (session/weekly headline + local contributing breakdown). The app shows it in a usage sheet. |
| `usage_estimate`| `{ "calibrated": true, "session_est_pct": 52.3, "week_est_pct": 43.1, "session_real_pct": 40, "week_real_pct": 41, "cum_tokens": 12345678, "tokens_since_check": 456789, "turns_since_check": 14, "last_check_at": <unix s>, "bench_set": true, "bench_sess_pct": 40, "bench_week_pct": 41, "bench_tokens": 12000000, "tokens_since_set": 345678 }` | the server-global **drift-live usage estimate**, aggregated across ALL sessions and clients. `*_est_pct` drift up every turn (from summed weighted token cost, using a tokens-per-percent rate learned from successive `/usage` calibrations); `*_real_pct` are the last `/usage` calibration's true numbers. `-1` on the `*_pct` fields (or `calibrated: false`) means no `/usage` anchor yet, so no estimate. The `bench_*` fields describe an armed manual benchmark (`usage_set`): the percentages/odometer it was stamped at and `tokens_since_set` burned since, which `usage_calc` divides by the percent gained to set the rate directly. Emitted after **every turn** (drift), after a `/usage` calibration (snap to real), and pushed once on connect. The app shows the estimate in the drawer footer + usage sheet. |
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
srv -> { "type": "say", "text": "ok, made that directory. want to attach?" }
srv -> { "type": "dialog", "state": "await_attach", "prompt": "want to attach?" }

app -> { "type": "wake" }
app -> <binary audio...>
app -> { "type": "audio_end" }
srv -> { "type": "transcript", "text": "yes", "final": true }
srv -> { "type": "attached", "name": "claude-claude" }
```

## Error codes

| code               | meaning                                                  |
|--------------------|----------------------------------------------------------|
| `unauthorized`     | bad/missing token                                        |
| `bad_message`      | malformed/unparseable client message                     |
| `bad_path`         | spawn path escaped allowed root                          |
| `bad_adopt`        | invalid `adopt` request                                  |
| `bad_delete`       | invalid `delete`/`delete_discovered` request             |
| `bad_rename`       | invalid `rename`/`rename_discovered` request             |
| `spawn_failed`     | session directory creation / claude failed to start      |
| `no_session`       | action referenced an unknown session                     |
| `not_found`        | referenced directory/session not found                   |
| `not_implemented`  | audio path invoked but STT is disabled (no whisper)      |
| `session_active`   | refused: an interactive `claude` is live in a terminal   |
| `discover_failed`  | scanning `~/.claude/projects` failed                     |
| `history_failed`   | reading a session transcript failed                      |
| `rename_failed`    | rename could not be persisted                            |
| `transcribe_failed`/`whisper_failed` | STT engine error                       |
| `turn_failed`      | the dictation turn errored (non-success `result`)        |
| `compress_failed`  | the `compress` summarization turn errored                |
| `usage_failed`     | running `/usage` to fetch the plan's usage report failed |
| `internal`         | unexpected server error                                  |

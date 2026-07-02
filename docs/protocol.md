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
| `delete_discovered` | `{ "session_id": "<uuid>" }`          | PERMANENTLY delete a session's Claude transcript from disk (and its registry record, if any). Refused with `session_active` if the session is live in a terminal. -> refreshed `discovered` + `session_list` |
| `rename`        | `{ "name": "<old>", "new_name": "<new>" }`| rename a session (keeps its session_id) -> `session_list` |
| `delete`        | `{ "name": "<session>" }`                 | delete a session record -> `session_list`     |
| `browse`        | `{ "path": "<dir or empty>" }`            | list a directory for the New-session picker (empty = roots) -> `listing` |
| `spawn_at`      | `{ "path": "<dir>" }`                     | create a session in `path` and attach -> `attached` + `session_list` |
| `history`       | `{ "name": "<session>", "before": <int?>, "limit": <int> }` | request a page of that session's past conversation (from Claude's transcript). `before` = exclusive index cursor (omit for the most recent page; page older by passing the oldest index held). -> `history` |
| `cancel`        | `{}`                                      | abort current dialog                          |
| `ping`          | `{}`                                      | keepalive                                     |

Audio framing: client sends `wake` (with a `codec`, and optional `hands_free`), then binary audio,
then `audio_end`. The server assembles the bytes, decodes to WAV, transcribes (whisper.cpp), then:

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

`hello` carries `end_token` (the word that commits a hands-free message). The server sends
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
| `hello_ok`      | `server_version`, `session_id`                       | auth accepted                            |
| `transcript`    | `{ "text": "...", "final": true }`                   | STT result (may stream partials)         |
| `say`           | `{ "text": "ok bud, where do you want it?" }`        | app should speak this (TTS) + display    |
| `dialog`        | `{ "state": "await_dir", "prompt": "..." }`          | current dialog state (drives the UI)     |
| `session_list`  | `{ "sessions": [{ "name", "dir", "attached" }] }`    | response to `list`                       |
| `attached`      | `{ "name": "claude-xyz" }`                            | now in passthrough mode                  |
| `detached`      | `{}`                                                  | left passthrough mode                    |
| `output`        | `{ "name": "...", "text": "...", "chunk": true }`     | clean session output (for display + TTS) |
| `history`       | `{ "name": "...", "messages": [{ "index", "role": "user"\|"claude", "text" }], "more": true }` | a page of past conversation (oldest→newest); `more` = older messages remain to page in. Response to `history`. |
| `read_last`     | `{ "count": <int> }`                                 | app re-reads (TTS) + scrolls to the last `count` Claude replies in the current session (from the `read last X` command) |
| `discovered`    | `{ "sessions": [{ "name", "dir", "session_id", "last_active": <unix s>, "active": <bool>, "registered": <bool> }] }` | all Claude sessions found on disk (one per dir, newest first). `active` = an interactive `claude` is open in tmux at that dir (driving it then risks a two-writer conflict); `registered` = already in the store. Response to `discover`. |
| `error`         | `{ "code": "...", "message": "..." }`                 | spoken/displayed error feedback          |
| `pong`          | `{}`                                                  | keepalive reply                          |

## Output path note

`output` messages must carry **clean text only** — ANSI codes, spinners, and TUI redraws stripped
server-side (see the TUI-capture decision in `CLAUDE.md`). The app should be able to feed
`output.text` straight to TTS. Use `chunk: true` for incremental streaming and a final message
with `chunk: false` to mark the end of a response turn.

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

| code            | meaning                                  |
|-----------------|------------------------------------------|
| `unauthorized`  | bad/missing token                        |
| `bad_path`      | spawn path escaped allowed root          |
| `spawn_failed`  | tmux/claude failed to start              |
| `no_session`    | attach/kill referenced unknown session   |
| `internal`      | unexpected server error                  |

# claude_spawner

A voice-driven remote control for [Claude Code](https://claude.com/claude-code).

Speak to an **Android app**, and it relays your voice to a **server** on your machine that
spawns and manages **tmux sessions** running Claude Code. The app is a hands-free passthrough:
say a command and it's executed; attach to a session and your dictation goes straight to Claude,
with Claude's replies streamed back and read aloud.

## How it works

You start every command with the wake word **"hey buddy"**:

```
You:   "hey buddy, spawn a new session"
App:   "ok bud, where do you want it?"
You:   "in data claude underscore claude"
App:   "ok, made that directory. want to attach?"
You:   "yes"
App:   (attached — now everything you say is dictated to Claude Code)
```

- **Wake word** "hey buddy" is detected **on your phone** (Porcupine / Picovoice).
- **Speech-to-text** runs on the **server** (Whisper) for accuracy.
- The **server** drives Claude Code **headless** (`claude -p --output-format stream-json`) with
  `--dangerously-skip-permissions`. A session is a durable `session_id` on disk, reattached each
  turn with `--resume` — so responses come back as clean structured text, never scraped from a
  terminal UI.
- While **attached**, your speech is dictated into the session and Claude's response is
  streamed back to the phone (display + text-to-speech).
- Optionally, you can `tmux attach` to the **same** session on a real terminal to watch or take
  over the interactive TUI.

## Stack

| Part        | Choice                                                              |
|-------------|---------------------------------------------------------------------|
| Server      | **Go** — WebSocket gateway, tmux session manager, Whisper glue      |
| Android app | **Kotlin** — Porcupine wake word, audio capture, TTS, WS client     |
| Wake word   | **On-device** (Porcupine)                                           |
| STT         | **Server-side Whisper** (hybrid: wake on-device, dictation on server)|
| Sessions    | **headless `claude -p` (stream-json)**, durable via `session_id` on disk |
| Babysit view| **tmux** running `claude --resume <id>` — optional, same session         |

See [`CLAUDE.md`](./CLAUDE.md) for the full architecture, data flow, and design notes.

## Reserved commands (planned)

All prefixed with **"hey buddy"**:

- `spawn a new session` — interactive dialog for directory + attach
- `attach to <name>`
- `detach`
- `list sessions`
- `kill session <name>`
- `what's the status` / `what's it doing`

Anything spoken **while attached** that isn't a reserved command is dictated to the session.

## How responses are captured (the once-hard problem, now solved)

Claude Code's interactive TUI would be miserable to screen-scrape (ANSI, redraws, spinners), so
we **don't**. The server drives Claude **headless** in `stream-json` mode and reads the clean
`result` event for each turn — verified end-to-end against `claude` 2.1.196, including multi-turn
memory via `--resume`. Details in [`CLAUDE.md`](./CLAUDE.md#-resolved-how-we-capture-claudes-responses-do-not-scrape-the-tui).

## Security note

The server can run arbitrary commands (Claude runs with permissions bypassed). **Do not expose
it to the internet without authentication and TLS.** Use a private network / Tailscale, require
an auth token from the app, and constrain spawn directories.

---

## Run it in Docker (recommended)

The whole execution environment — Go, `tmux`, the `claude` CLI, and **whisper.cpp + a model** —
is baked into a container so nothing has to be installed on the host. Source stays on the host
(bind-mounted), so you edit and version normally; only execution is containerized.

```bash
# build (compiles whisper.cpp + fetches a model; base.en by default) and run on :8080
docker compose up --build

# drive it with the text client (from inside the same environment)
docker compose run --rm spawner go run ./cmd/wsclient -url ws://spawner:8080/ws
#   hey buddy spawn a new session
#   workspace demo
#   yes
#   <anything> -> dictated to Claude Code; reply comes back as 💬

# test real voice transcription end-to-end with whisper's sample clip
docker compose run --rm spawner \
  go run ./cmd/wsclient -url ws://spawner:8080/ws -audio /opt/whisper.cpp/samples/jfk.wav
```

- `claude` inside the container authenticates via your host creds, mounted from `~/.claude` +
  `~/.claude.json` (or set `ANTHROPIC_API_KEY` in your shell). See `docker-compose.yml`.
- Sessions are spawned under `/workspace` (a persisted volume); `SPAWNER_ROOT` jails them there.
- Bigger/more-accurate model: `docker compose build --build-arg WHISPER_MODEL=small.en`.

## Try it on the host (no Docker)

```bash
cd server
mkdir -p /tmp/sandbox
SPAWNER_TOKEN=secret SPAWNER_ROOT=/tmp/sandbox go run .          # text path only
# add SPAWNER_WHISPER_MODEL=/path/to/ggml-small.en.bin to enable voice
```

Then in another terminal, `SPAWNER_TOKEN=secret go run ./cmd/wsclient` and type utterances.
Requires the `claude` CLI and `tmux` on the host. Transcription is **off** unless
`SPAWNER_WHISPER_MODEL` is set (text utterances work either way). Config vars:
`SPAWNER_WHISPER_BIN` (default `whisper-cli`), `SPAWNER_WHISPER_MODEL`, `SPAWNER_WHISPER_LANG`
(`en`). Audio in is PCM16LE / 16 kHz / mono (see `docs/protocol.md`). Engine rationale is in
[`CLAUDE.md`](./CLAUDE.md).

---

## To-Do / Roadmap

### Phase 0 — Decisions & spec ✅
- [x] Response-capture decision: **headless `claude -p --output-format stream-json`**, durable
      `session_id` on disk + `--resume` — verified end-to-end. (No TUI scraping.)
- [x] `docs/protocol.md` — WebSocket message schema
- [x] `docs/commands.md` — "hey buddy" command grammar + dialog flows
- [ ] Decide auth mechanism (shared token is scaffolded; consider mTLS) and transport
      (Tailscale? reverse proxy?)

### Phase 1 — Server skeleton (Go)
- [x] Project scaffold (`/server`), env config, graceful shutdown, `/healthz`
- [x] `internal/session`: headless `claude` driver (`Driver.Turn`, stream-json parsing,
      tool-event breadcrumbs, error subtypes) + `NewSessionID`
- [x] `internal/session`: durable, file-backed session registry (`Store`) with atomic writes
- [x] `internal/tmux`: optional human-babysit pane (`Babysit`/`List`/`Exists`/`Close`)
- [x] Spawn-path validation against an allowed root (`config.ValidateSpawnDir`, tested)
- [x] WebSocket gateway + auth handshake (`internal/gateway`, gorilla/websocket)
- [x] `spawn` action: generate `session_id`, mkdir (validated), persist record
- [x] Wire `list` / `attach` / `detach` / `kill` actions to the store + driver
- [x] Dictation turn loop: attached utterance → `Driver.Turn` → `output` (async, tool breadcrumbs)
- [x] `cmd/wsclient`: interactive text client for manual testing (no app needed)
- [x] **Verified live**: spawn → attach → dictate → real claude reply → `--resume` recall

### Phase 2 — Transcription & dialog
- [x] Command parser: control command vs passthrough dictation (`internal/command`)
- [x] Dialog state machine (the "ok bud, where?" → "want to attach?" flow, `internal/gateway`)
- [x] Spoken-path normalization (`internal/pathspeak`; note: lowercases — see CLAUDE.md)
- [x] Audio-stream ingest: `wake` + binary PCM16 frames + `audio_end` → WAV → transcript → `utterance`
- [x] Whisper integration behind a `Transcriber` interface (`internal/transcribe`, whisper.cpp shell-out)
- [x] Dockerized dev environment (Go + tmux + claude CLI + whisper.cpp + model)
- [ ] Verify a real audio turn end-to-end in the container (jfk.wav → transcript)
- [ ] Vocab biasing (`--prompt`) to improve recognition of session names / paths

### Phase 3 — Android app (Kotlin / Compose)
- [x] Project scaffold (`/android`), mic permission, foreground-service stub
- [x] Compose UI: connect, push-to-talk, text-utterance input, conversation log
- [x] WebSocket client (OkHttp) speaking the protocol; hello/auth handshake
- [x] Audio capture (PCM16/16k/mono) streamed over the voice path (wake→frames→audio_end)
- [x] Receive transcripts / dialog / session output; TTS playback of say/output
- [x] **Verified live on emulator**: app → server → real Claude reply, full spawn/attach/dictate
- [ ] Porcupine "hey buddy" wake-word integration (stub in `wake/WakeWordController.kt`)
- [ ] Move wake listener into `VoiceService` for background always-listening

### Phase 4 — Passthrough & attach
- [ ] Attach = bind voice I/O to a session; dictation becomes the prompt for `Driver.Turn`
- [ ] Stream the `result` text (and tool breadcrumbs) back to the app as `output` messages
- [ ] Detach / switch sessions (just changes which session_id turns target)
- [ ] Read Claude's responses aloud (the `result` text is already clean)
- [ ] Optional: open a tmux babysit pane on demand ("hey buddy, let me watch")

### Phase 5 — Polish
- [x] Auto-connect on launch + auto-reconnect with backoff; resume last session (client-driven,
      survives server restart) and in-progress spawn dialog (server-side, keyed by `client_id`)
- [x] Barge-in: push-to-talk / "hey bud stop" interrupts TTS; markdown stripped from speech
- [ ] Multiple concurrent sessions + quick switching by voice
- [ ] Error handling & spoken error feedback ("that directory doesn't exist, bud")
- [ ] Barge-in (interrupt TTS by speaking)
- [x] Persist session list across server restarts (durable `session_id`s in the store)

### Nice-to-have / later
- [ ] Per-session naming by voice
- [ ] Notifications when a backgrounded session finishes / needs input
- [ ] On-device fallback STT when offline
- [ ] iOS app

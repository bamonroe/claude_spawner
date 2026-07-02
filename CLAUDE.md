# CLAUDE.md

Guidance for Claude Code instances working in this repository.

## What this project is

**claude_spawner** is a voice-driven remote control for Claude Code. It has two halves:

1. **Android app** (Kotlin) — listens for the wake word **"hey buddy"**, captures voice,
   and acts as a passthrough terminal to remote Claude Code sessions.
2. **Server** (Go) — runs on the user's machine, spawns and manages **tmux** sessions with
   Claude Code running inside them, and bridges voice/text between the app and those sessions.

The user speaks; the app transcribes (via server-side Whisper); the text is either interpreted
as a **reserved control command** or passed through to the currently attached Claude Code
session. Claude Code's output is streamed back to the phone and read aloud (TTS).

## The "hey buddy" command grammar

Every control command is prefixed with the wake word **"hey buddy"**. Anything spoken while
attached to a session that is *not* a recognized control command is treated as **dictation**
and forwarded to that session verbatim.

Example flow:

```
User:  "hey buddy, spawn a new session"
App:   "ok bud, where do you want it?"
User:  "in data claude underscore claude"        ->  /data/claude_claude
App:   "ok, made that directory. want to attach?"
User:  "yes"
App:   (attaches; subsequent speech is dictated into the session)
```

Reserved commands live server-side as a parseable grammar (see `docs/commands.md` once created).
The wake word is detected **on-device** (Porcupine); everything after it is streamed to the
server for transcription and parsing. Keep the wake word and the command vocabulary in **one
authoritative place** so the app and server agree.

## Architecture & data flow

```
┌─────────────────────────┐         WebSocket          ┌──────────────────────────────┐
│   Android app (Kotlin)  │ ─── audio / control ─────> │        Server (Go)           │
│  - Porcupine wake word  │                            │  - WebSocket gateway         │
│    ("hey buddy")        │ <── transcript / output ── │  - Whisper transcription     │
│  - audio capture        │                            │  - command parser/dialog FSM │
│  - TTS playback         │                            │  - session driver + store    │
│  - session UI           │                            │                              │
└─────────────────────────┘                            └──────────────┬───────────────┘
                                                                       │ claude -p --resume <id>
                                                                       │ --output-format stream-json
                                                                       v
                                                        ┌──────────────────────────────┐
                                                        │ headless claude (per turn)   │
                                                        │  -> NDJSON: assistant / tool │
                                                        │     / result  (clean text)   │
                                                        │  state persists to disk via  │
                                                        │  session_id (no live proc)   │
                                                        └──────────────────────────────┘
                                  (optional) human babysit: tmux pane running
                                  `claude --resume <id>` attached to the SAME session_id
```

- **Wake word**: on-device via Porcupine (Picovoice). Low latency, no audio leaves the phone
  until the wake word fires.
- **Transcription (STT)**: server-side Whisper (whisper.cpp or a local Whisper service). The app
  streams captured audio after the wake word; the server returns a transcript.
- **Transport**: a single WebSocket per app session carries audio up and transcripts/session
  output down. Use REST only for stateless control actions if needed.
- **Session control**: the server shells out to `claude` headless (see the RESOLVED section
  below). Input is the prompt arg; output is parsed from `stream-json`. tmux is only the optional
  human-babysit view, not the data path.

## ✅ RESOLVED: how we capture Claude's responses (do NOT scrape the TUI)

The original worry was that Claude Code in tmux is a full-screen TUI (ANSI, redraws, spinners),
so reading its output for TTS looked painful. **We do not scrape the TUI at all.** Decision,
validated end-to-end against `claude` 2.1.196:

> Drive Claude Code **headless** in `stream-json` mode. A "session" is a durable
> **`session_id` on disk tied to a directory**, not a live process. Each dictated turn shells
> out to `claude`, and the clean `result` event is the text we speak.

Per-turn invocation (working dir = the session's directory):

```
claude -p "<transcribed text>" \
  --session-id <uuid>      # FIRST turn: we generate the uuid ourselves
  # --resume <uuid>        # LATER turns: reattach instead of --session-id
  --output-format stream-json --verbose \
  --dangerously-skip-permissions
```

Parsing stdout (newline-delimited JSON):
- `type:"system"` (init), `type:"assistant"`, `type:"user"` (tool results), `type:"rate_limit_event"` — ignore for TTS.
- `event.type:"content_block_start"` with `content_block.type:"tool_use"` → optional spoken
  breadcrumb ("running Bash…"), using `content_block.name`.
- **`type:"result"`** → `result` is the clean final answer to speak; `session_id` confirms the id;
  `subtype` is `"success"` or `"error_*"` (treat non-success / `is_error` as a failed turn).

For TTS we take the **final `result`**, not token deltas — TTS wants whole sentences.
`--include-partial-messages` (requires `--verbose`) gives `text_delta` events if we later want
live on-screen streaming, but it is not needed for the voice path.

This is implemented in `internal/session` (`Driver.Turn`, `Store`, `NewSessionID`) and was
verified: turn 1 with `--session-id` then turn 2 with `--resume` correctly retained context.

### tmux is now OPTIONAL — the human-babysit view only

Because the session is a `session_id` on disk, **two views can attach to the same conversation**:

| View          | How                                                      | When                    |
|---------------|---------------------------------------------------------|-------------------------|
| Voice path    | headless `claude -p --resume <id> --output-format stream-json` | every dictated turn |
| Human babysit | interactive `claude --resume <id>` inside a tmux pane   | to watch/take over      |

`internal/tmux` now only opens the babysit pane (`Babysit`/`List`/`Exists`/`Close`). **One active
writer per session at a time** — don't run a headless turn and a babysit pane against the same
`session_id` simultaneously.

## Security posture

- Claude Code runs with `--dangerously-skip-permissions`. This is **intentional** per the user,
  but it means the server can execute arbitrary commands on the host. Treat the server as
  privileged.
- The server must **authenticate** the app (token/mTLS) before accepting any command — anyone
  who can reach the WebSocket can spawn unrestricted Claude sessions.
- Never expose the server to the public internet without auth + TLS. Prefer a private network
  / Tailscale / reverse proxy with auth.
- Validate and constrain directory paths from "spawn" commands (no surprise traversal outside an
  allowed root unless the user opts in).

## Repository layout

```
/server                         Go server (module: github.com/bam/claude_spawner/server)
  main.go                       entrypoint: HTTP server, graceful shutdown, /healthz, /ws
  internal/gateway/             WebSocket gateway: auth, dispatch, dialog FSM, dictation loop
    gateway.go                  Server, conn, auth handshake, read loop
    ops.go                      control commands (list/attach/detach/kill/status) + dictate
    dialog.go                   spawn dialog FSM, session creation, name sanitizing
    messages.go                 wire message constructors
    gateway_test.go             httptest+ws integration (auth, spawn dialog, dictation)
    audio.go                    audio path: wake/binary-PCM16/audio_end -> WAV -> STT -> utterance
  internal/session/session.go   headless claude driver: Driver.Turn (stream-json), NewSessionID
  internal/session/store.go     durable session registry (file-backed, atomic writes)
  internal/command/command.go   utterance -> intent parser + StripWake
  internal/pathspeak/           spoken path -> filesystem path normalizer
  internal/transcribe/          Transcriber interface + whisper.cpp shell-out + PCM16->WAV
  internal/tmux/tmux.go         OPTIONAL human-babysit pane (Babysit/List/Exists/Close)
  internal/config/config.go     env config + spawn-path validation
  cmd/wsclient/main.go          text client for manual testing; -audio streams a WAV
  Dockerfile / .dockerignore    dev image: Go + tmux + claude CLI + whisper.cpp + model
docker-compose.yml              dev orchestration (bind-mounts source, mounts host claude auth)
/android                        Android app (Kotlin) — placeholder, see android/README.md
/docs
  protocol.md                   WebSocket message schema (single source of truth)
  commands.md                   "hey buddy" command grammar + dialog flows
README.md / CLAUDE.md / .gitignore
```

Status: the **entire server-side voice pipeline works and is verified live** against `claude`
2.1.196 — spawn dialog → mkdir → attach → dictation turn → real reply → `--resume` recall across
reconnects. Server-side Whisper (audio → `transcript` → `utterance`) is wired and unit-tested
with fakes. Everything builds/vets/tests clean (`go test ./...`). What remains: install
whisper.cpp + a model on the host to verify a real *audio* turn (the shell-out contract is
tested with a fake binary), and the Android app. Update this section as code lands.

The text seam: the app sends an `utterance` message with already-transcribed text. The audio path
(`wake` → binary PCM16 frames → `audio_end`) assembles a WAV, runs the Transcriber, emits a
`transcript`, then feeds the text through that exact same seam — so the command/dialog/turn
machinery is engine-agnostic and was fully exercised before STT existed.

### Transcription (internal/transcribe)

Engine choice for THIS host (4-core Skylake, no CUDA GPU): **whisper.cpp + `small.en`**, CPU/int8,
utterance-based (not streaming). Rationale: no GPU nullifies faster-whisper's edge; whisper.cpp is
a single binary we `exec` like `claude`/`tmux` (no Python sidecar); fully local. The gateway
depends only on the `Transcriber` interface, so swapping to faster-whisper or a cloud API (e.g.
Groq large-v3-turbo) is a one-file change. Disabled unless `SPAWNER_WHISPER_MODEL` is set; when
disabled the audio path returns `not_implemented` but text utterances still work.

Known limitation: `pathspeak` lowercases spoken paths (STT output is lowercase), so sessions
can't be created in directories with uppercase letters by voice. Acceptable; documented in
docs/commands.md.

### Build & run the server

Preferred: **Docker** (bundles claude CLI + tmux + whisper.cpp + model; nothing installed on the
host). Source is bind-mounted, so host edits apply on the next `go run`.

```
docker compose up --build                                  # server on :8080, STT enabled
docker compose run --rm spawner go run ./cmd/wsclient -url ws://spawner:8080/ws
docker compose run --rm spawner \
  go run ./cmd/wsclient -url ws://spawner:8080/ws -audio /opt/whisper.cpp/samples/jfk.wav
```

Host-native (needs claude, tmux, and optionally whisper-cli locally):

```
cd server
go build ./... && go test ./...
SPAWNER_TOKEN=<secret> SPAWNER_ROOT=/data go run .   # refuses to start without SPAWNER_TOKEN
```

The container mounts host `~/.claude` + `~/.claude.json` so the in-container `claude` uses your
OAuth login (or set `ANTHROPIC_API_KEY`). `go.mod` targets go 1.23 so the stock `golang:1.23`
image works (host has newer; both satisfy it).

Config env vars: `SPAWNER_ADDR` (`:8080`), `SPAWNER_TOKEN` (required), `SPAWNER_ROOT` (spawn-dir
jail), `SPAWNER_STATE` (`sessions.json`), `SPAWNER_CLAUDE_BIN` (`claude`), `SPAWNER_WHISPER_BIN`
(`whisper-cli`), `SPAWNER_WHISPER_MODEL` (path; enables STT), `SPAWNER_WHISPER_LANG` (`en`).

## Conventions

- Keep the **command grammar** and the **WebSocket message protocol** in `/docs` as the single
  source of truth; both client and server reference it.
- Server: idiomatic Go, `gofmt`, errors wrapped with context. Keep tmux interaction behind one
  package so the shell-out details are isolated and testable.
- Android: Kotlin, keep audio/wake-word, networking, and UI in separate modules/packages.
- When you change the architecture or make a design decision (especially the TUI-capture
  question above), record it in this file and the README so it isn't re-litigated.

### Git: commit after every change, atomically

This repo is under version control (remote `origin` = `git@github:bamonroe/claude_spawner`, using
the `github` SSH host alias). **Commit after every major OR minor change** — don't let work pile up
uncommitted (a whole session was once built with no repo at all; never again).

- **Atomic commits**: one logical change per commit. A bug fix, a feature, a doc update, and a
  refactor are separate commits — don't bundle unrelated changes. Commit the smallest coherent unit
  that builds/tests clean.
- Make the change → build/vet/test it (`go build ./... && go test ./...`, or the APK build) → commit.
- Write a concise imperative subject (`fix: input bar behind nav bar`, `feat: read-last command`).
- Prefer many small commits over one large one; it keeps history bisectable and easy to revert.

## Current status

Greenfield. Nothing is built yet — see the **To-Do / Roadmap** in `README.md` for the plan and
ordering. Phase 0 (decisions + protocol spec) should be settled before writing app/server code.

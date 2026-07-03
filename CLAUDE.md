# CLAUDE.md

Guidance for Claude Code instances working in this repository.

## Start here: the documentation map

This repo keeps documentation **de-duplicated** — every fact has exactly **one** authoritative
home. When you need to know or change something, go to its owner below; don't restate a fact in a
second file (link to the owner instead). This table is itself the index: read it first.

| You want to know / change…                    | Authoritative home                          | Enforced by |
|-----------------------------------------------|---------------------------------------------|-------------|
| **What to do next / what's done** (task state)| `TODO.md`                                   | discipline (the `TODO.md` rule below) |
| **How the system works** (architecture, decisions, conventions, how to work here) | `CLAUDE.md` (this file) | discipline |
| **How a user runs/uses it** (setup, security, phase history) | `README.md`                     | discipline |
| **WebSocket wire protocol** (every message + error code) | `docs/protocol.md`               | `internal/docsync` tests |
| **"hey buddy" command grammar**               | `docs/commands.md` (prose) + `command.Registry` (code) → `docs/commands.json` (generated) | `internal/command` + `cmd/gencommands` |
| **Config env vars** (`SPAWNER_*`)             | `CLAUDE.md` (config section) — code owns them in `internal/config` | `internal/docsync` tests |

**Two classes of fact, two ways they're kept honest:**

1. **Code-derived facts** (env vars, wire messages, error codes, the command list) are owned by
   the code. The docs are a mirror, and a **drift test fails the build** if they fall out of sync:
   - `internal/command` ↔ `docs/commands.json` (regenerate with `go run ./cmd/gencommands`);
   - `internal/docsync` ↔ `docs/protocol.md` + `CLAUDE.md` (env vars, in/outbound messages, error
     codes) — see that package's doc comment. A red `go test ./...` names exactly what's stale.
   So: **change the code, then `go test ./...` tells you which doc to update.** Never hand-maintain
   a second copy the tests don't check. (Go caches test results on Go-source inputs, not the
   Markdown files — a code change always re-runs the checks; for a **doc-only** edit run the
   canonical drift check uncached: `go test ./... -count=1`.)
2. **Narrative facts** (status, "verified live", roadmap history) can't be tested, so they live in
   **one** place only — status/tasks in `TODO.md`, architecture here, run/history in `README.md` —
   and the update rules below (and in `README.md`) keep that single copy current.

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
                                  tmux is inspected only to detect a `claude` a
                                  human already has open in a pane (conflict warning)
```

- **Wake word**: on-device via Porcupine (Picovoice). Low latency, no audio leaves the phone
  until the wake word fires.
- **Transcription (STT)**: server-side Whisper (whisper.cpp or a local Whisper service). The app
  streams captured audio after the wake word; the server returns a transcript.
- **Transport**: a single WebSocket per app session carries audio up and transcripts/session
  output down. Use REST only for stateless control actions if needed.
- **Session control**: the server shells out to `claude` headless (see the RESOLVED section
  below). Input is the prompt arg; output is parsed from `stream-json`. tmux is not on the data
  path — it is inspected only to notice a `claude` a human already has open interactively.

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

### tmux is used only to detect a live interactive `claude`

Because the session is a `session_id` on disk, a human could also `claude --resume <id>` it in a
terminal. `internal/tmux` exposes just `ClaudeDirs` — the set of directories with an interactive
`claude` open in a pane — so the spawner can warn before driving that same session headlessly.
**One active writer per session at a time** — don't run a headless turn against a `session_id` a
human is editing live. (An earlier design had the server itself open a "babysit" pane via a
`Babysit`/`List`/`Exists`/`Close` API; that was dropped — the server never creates panes now.)

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
    gateway.go                  Server, conn, auth handshake, read loop, message dispatch
    ops.go                      control commands (list/attach/detach/kill/status) + dictate
    dialog.go                   spawn dialog FSM, session creation, name sanitizing
    audio.go                    audio path: wake/binary/audio_end -> WAV -> STT -> utterance
    stream.go                   hands-free streaming: live pending draft, end-token commit
    jobs.go                     running-turn tracking: activity/files breadcrumbs, diff summary
    inflight.go                 per-session in-flight turn registry (abort, restart interrupts)
    ask.go                      interactive-mode clarifying-question (ask) extraction
    browse.go                   directory listing for the New-session picker (listing)
    messages.go                 wire message constructors
    *_test.go                   httptest+ws integration (auth, spawn, dictation, ask, stream)
  internal/session/session.go   headless claude driver: Driver.Turn (stream-json), NewSessionID
  internal/session/store.go     durable session registry (file-backed, atomic writes)
  internal/session/discover.go  scan ~/.claude/projects for all Claude sessions (adopt/discover)
  internal/session/transcript.go read/stitch on-disk transcripts for `history` (spans clears)
  internal/command/command.go   utterance -> intent parser + StripWake
  internal/command/registry.go  Command registry (single source of truth) + RegistryJSON
  internal/transcribe/          Transcriber interface: WhisperCPP (CLI) + RemoteWhisper (HTTP)
  internal/projects/projects.go spoken-path fuzzy matching against the spawn roots
  internal/tmux/tmux.go         detect a live interactive `claude` in a pane (ClaudeDirs)
  internal/config/config.go     env config + spawn-path validation
  cmd/wsclient/main.go          text client for manual testing; -audio streams a WAV
  cmd/gencommands/main.go       regenerate docs/commands.json from the command registry
  Dockerfile / .dockerignore    dev image: Go + tmux + claude CLI + whisper.cpp CLI + model
docker-compose.yml              dev orchestration: spawner + resident whisper/whisper-fast servers
/whisper                        Vulkan/CPU Dockerfiles for the resident whisper.cpp server (see whisper/README.md)
/deploy                         host systemd unit + env example + claude-log helper (see deploy/README.md)
/android                        Android app (Kotlin/Compose) — see android/README.md
/docs
  protocol.md                   WebSocket message schema (single source of truth)
  commands.md                   "hey buddy" command grammar + dialog flows
  commands.json                 command list generated from the registry (consumed by the app build)
README.md / CLAUDE.md / TODO.md / .gitignore
```

Status: the **full voice loop works end-to-end and is verified live** against `claude` 2.1.196 —
spawn dialog → mkdir → attach → dictation turn → real reply → `--resume` recall across reconnects.
Real **audio** turns are verified too: a spoken/`jfk.wav` clip → Whisper → `transcript` →
`utterance` → Claude reply, on both the resident GPU whisper server and the CLI fallback (the
shell-out contract is also unit-tested with a fake binary). The **Android app** is built and
verified live on the emulator and the Pixel 8a. The live-tracked to-do list is in `TODO.md`.

The text seam: the app sends an `utterance` message with already-transcribed text. The audio path
(`wake` → binary PCM16 frames → `audio_end`) assembles a WAV, runs the Transcriber, emits a
`transcript`, then feeds the text through that exact same seam — so the command/dialog/turn
machinery is engine-agnostic and was fully exercised before STT existed.

### Transcription (internal/transcribe)

The gateway depends only on the `Transcriber` interface; there are **two implementations** and
either can back it:

- **`RemoteWhisper`** (`remote.go`) — POSTs the WAV to a **resident whisper.cpp HTTP server**
  (`/inference`). This is the preferred path on this host, which has an **AMD RX 550 GPU**: the
  `whisper`/`whisper-fast` compose services run whisper.cpp built with **Vulkan** and keep the
  model warm. Two servers run: an accurate model (`medium.en`, `:8571`) for real dictation, and a
  fast draft model (`base.en`, `:8572`) for the live hands-free draft + end-token detection, so
  the cheap high-frequency work never blocks the accurate model. Enabled via
  `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL`. (Measured on the RX 550: `medium.en` ~4.8s,
  `small.en` ~2–3s, `large-v3` ~10.5s per clip — 3–4× the CPU-only build.)
- **`WhisperCPP`** (`transcribe.go`) — shells out to the **whisper.cpp CLI** (one process per
  utterance), `exec`'d like `claude`/`tmux`, no server. The fallback when no whisper URL is set.
  It size-picks a model per clip (tiny/base/small) from `SPAWNER_WHISPER_MODEL{,_FAST,_BASE}`.

Opus clips are decoded to 16 kHz mono WAV with **ffmpeg** first (whisper can't read Opus). STT is
disabled unless a model/URL is configured; when disabled the audio path returns `not_implemented`
but text utterances still work. Swapping to faster-whisper or a cloud API (e.g. Groq
large-v3-turbo) stays a one-file change behind the `Transcriber` interface.

Known limitation: STT output is all-lowercase, so sessions can't be created in directories with
uppercase letters by voice. Acceptable; documented in docs/commands.md.

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

Config env vars (all read in `internal/config`):

- `SPAWNER_ADDR` (`:8080`), `SPAWNER_TOKEN` (**required**), `SPAWNER_ROOT` (colon-separated
  spawn-dir jail), `SPAWNER_STATE` (`sessions.json`), `SPAWNER_CLAUDE_BIN` (`claude`).
- CLI STT: `SPAWNER_WHISPER_BIN` (`whisper-cli`), `SPAWNER_WHISPER_MODEL` (path; enables STT),
  `SPAWNER_WHISPER_MODEL_FAST` / `SPAWNER_WHISPER_MODEL_BASE` (per-size model paths for the
  clip-length model picker), `SPAWNER_WHISPER_LANG` (`en`), `SPAWNER_FFMPEG_BIN` (`ffmpeg`).
- Resident-server STT: `SPAWNER_WHISPER_URL` (accurate server), `SPAWNER_WHISPER_FAST_URL` (fast
  draft/detection server), `SPAWNER_WHISPER_MODEL_NAME` (`medium.en`; reported to clients),
  `SPAWNER_WHISPER_FAST_MAX_SEC` (`2.5`; clips shorter than this use the fast server).

## Conventions

- Keep the **command grammar** and the **WebSocket message protocol** in `/docs` as the single
  source of truth; both client and server reference it.
- The **command set** has a code source of truth: `server/internal/command.Registry` (a list of
  `Command{Kind, Title, Aliases, Description, Example}` structs). Adding/changing a "hey buddy"
  command means editing that registry — tests enforce it (every `Example` must `Parse` to its
  `Kind`, and every user-facing `Kind` must be registered). Regenerate the shared artifact with
  `go run ./cmd/gencommands` (writes `docs/commands.json`); a drift test fails if it's stale. The
  Android build's `generateCommands` Gradle task turns that JSON into the app's alphabetical
  `COMMANDS` list at build time, so the app can never drift or ship an undocumented command. Do
  **not** hand-maintain a command list in the app.
- Server: idiomatic Go, `gofmt`, errors wrapped with context. Keep tmux interaction behind one
  package so the shell-out details are isolated and testable.
- Android: Kotlin, keep audio/wake-word, networking, and UI in separate modules/packages.
- When you change the architecture or make a design decision (especially the TUI-capture
  question above), record it in this file and the README so it isn't re-litigated.

### Git: commit atomically, at will and frequently

This repo is under version control (remote `origin` = `git@github:bamonroe/claude_spawner`, using
the `github` SSH host alias). **Commit atomically, at will, and frequently.** You have standing
authorization to commit your own work without asking first — don't wait to be told. Never let work
pile up uncommitted (a whole session was once built with no repo at all; never again).

- **Atomic commits**: one logical change per commit. A bug fix, a feature, a doc update, and a
  refactor are separate commits — don't bundle unrelated changes. Commit the smallest coherent unit
  that builds/tests clean.
- Make the change → build/vet/test it (`go build ./... && go test ./...`, or the APK build) → commit.
- Write a concise imperative subject (`fix: input bar behind nav bar`, `feat: read-last command`).
- Prefer many small commits over one large one; it keeps history bisectable and easy to revert.
- Commit freely and often — committing your own changes is never something you need to ask about.

### Document every feature immediately, in the same breath as writing it

**A feature isn't done until it's documented.** Write the documentation *during* the feature work,
or immediately after — never defer it to "later," and never ship code without it.

- Every new feature gets full user-facing documentation in `README.md` as part of the same work.
- Keep the single-source-of-truth docs in sync in the same pass: a new voice command goes in
  `docs/commands.md`, a new WebSocket message goes in `docs/protocol.md`.
- Docs land in the same commit as the feature (or an immediately-following commit) — a feature
  commit with no accompanying documentation is incomplete.

## Current status & tasks — see `TODO.md`

Project status ("what's built, what's next") lives in **one** place: **`TODO.md`**. It is not
restated here or in `README.md` (both link to it) so there is only ever one copy to update. The
short architectural "Status:" note under *Repository layout* above is the exception — it frames the
architecture, not the task list.

### `TODO.md` is the live task list — keep it current

`TODO.md` (repo root) is the **single source of truth for active and completed work**, separate
from the historical phase roadmap in `README.md`. **Update it in the same commit that changes the
work it describes:**

- **Propose a feature or a test** → add it to `TODO.md` (unchecked) as part of the same change.
- **Complete a feature or a test** → check it off (move it to the Done section, dated).
- **Delete or drop a test/feature** → remove or strike its entry, with a one-line why.

Treat a `TODO.md` edit as part of "done," exactly like the README/`docs/` documentation rule
above — a feature or test change with a stale `TODO.md` is incomplete.

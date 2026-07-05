# Architecture & internals

How claude_spawner works under the hood — the deep detail behind the one-line summary in
`CLAUDE.md`. Read this when you're changing the data path, the session driver, or transcription;
you don't need it for most turns. High-level "what it is" and the behavioral rules stay in
`CLAUDE.md`; user-facing setup/run and the narrative "how responses are captured" live in
`README.md`.

## Data flow

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
- **Session control**: the server shells out to `claude` headless (see below). Input is the prompt
  arg; output is parsed from `stream-json`. tmux is not on the data path — it is inspected only to
  notice a `claude` a human already has open interactively.

The **text seam**: the app sends an `utterance` message with already-transcribed text. The audio
path (`wake` → binary PCM16 frames → `audio_end`) assembles a WAV, runs the Transcriber, emits a
`transcript`, then feeds the text through that exact same seam — so the command/dialog/turn
machinery is engine-agnostic and was fully exercised before STT existed.

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

## Containerized server + per-session execution target (host vs sandbox)

Status: **implemented** (`internal/session/executor.go`, `internal/broker`, `cmd/broker`). Goal:
run the **server** in a container for clean deployment, while letting each spawned Claude session
run *either* directly on the host (real host files/toolchains) *or* inside an isolated container
sandbox (disposable, root-inside-the-sandbox) — chosen **per session** via `Session.Target`.

### The single seam

Every turn already funnels through one place: `session.Driver.Turn()` (`internal/session/
session.go`), which `exec`s the `claude` binary in the session's `Dir` and parses its
`stream-json` stdout. Nothing else in the server knows *how* that process is launched. So the whole
feature reduces to making that launch pluggable:

- Introduce an **`Executor`** interface (start a `claude` turn given `Dir` + args, return a stdout
  stream). The existing direct-`exec` becomes the `host` executor; a new `sandbox` executor runs
  the turn inside a container. `Turn()` selects one and is otherwise unchanged — the NDJSON parsing,
  `Setpgid` group-kill, and event fan-out all stay put.
- Add an **execution-target field** to the `Session` record (`store.go`), set at spawn time and
  persisted in `sessions.json`, so host-vs-sandbox is a durable per-session property the spawn
  dialog chooses. Default = `host` (today's behavior).

### The problem containerizing the server introduces

A process inside a container **cannot fork a process on the host**. So once the server is
containerized, "run this session directly on the host" needs a bridge back out. The privileged
shortcuts (a `--privileged` server with `--pid=host` + `nsenter` into PID 1) were considered and
**rejected**: they give the server container full host root, which defeats the point of isolating
it. We want *no component* to hold host root.

### The broker (host-execution without host root)

Run a small **host-side session broker** — a daemon on the host, as the ordinary user (not root),
listening on a Unix socket that's bind-mounted into the server container. The server sends it a
narrow, validated request ("start a `claude` turn in directory X"); the **broker** does the actual
`fork`/`exec` on the host and streams stdout back over the socket. Consequences:

- The **server container stays unprivileged** — it can only ask for the one constrained action the
  broker exposes, never arbitrary host commands.
- The **broker isn't root either** — it runs as the user and forks `claude` as the user, which is
  exactly the identity we want touching the user's files.
- The broker is the natural home for the **`SPAWNER_ROOT` jail** enforcement — it's the last hop
  before a host process launches, so path validation there is authoritative even if the server is
  compromised. (This is the tmux client/server split from the project's early thinking, generalized
  into a proper broker.)

### Sandbox sessions (also without host root)

For `sandbox`-target sessions the container's lifetime is **bound to the session**, not the turn:
the `SandboxExecutor` creates a long-lived container at spawn (`Ensure` → `run -d … sleep
infinity`, named `spawner-<hex>` from `Session.Container`), each turn runs via `exec -w <dir>`
into it, and it's destroyed when the session is deleted (`Remove` → `rm -f`). So packages
installed and services started in one turn persist to the next — a real environment, not a fresh
box per turn. `Ensure` is idempotent and re-run before every turn, so a container lost to a server
restart or manual `rm` is transparently recreated. Spawn-time `Ensure` is best-effort (logged, not
fatal); a hard runtime failure surfaces on the first turn. Use a **rootless Podman / rootless
Docker** runtime (`SPAWNER_SANDBOX_RUNTIME`) so none of this needs host root — the sandbox gets
root *inside itself* and a disposable FS. Session `Dir` is bind-mounted same-path (so the
transcript's project encoding matches the host); share `$HOME/.claude` via `SPAWNER_SANDBOX_MOUNTS`
to keep history/discovery working. Lifecycle hooks live in the gateway spawn (`ensureSandbox`) and
delete (`removeSandbox`) paths; `Driver.EnsureContainer`/`RemoveContainer` bridge to the executor.
At startup `Driver.ReconcileContainers` sweeps **orphans** — managed containers (matched by the
`spawner-` name prefix) whose session record no longer exists, e.g. deleted while the server was
down — so they don't accumulate; live sessions' containers are left for `Ensure`-before-turn.

### Net security posture

No component holds host root: server container = unprivileged; broker = plain user; sandboxes via a
rootless runtime. Cost is one new moving part (the broker daemon) plus the `Executor` seam. See
`docs/protocol.md` if a spawn-time target selector reaches the wire protocol (it may not — the
dialog can carry it server-side, like `rename`).

## Transcription (internal/transcribe)

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
uppercase letters by voice. Acceptable; documented in `docs/commands.md`.

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
  Dockerfile / .dockerignore    dev image: Go + claude CLI + whisper.cpp CLI + model (+ tmux for conflict detection)
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

Architectural status: the **full voice loop works end-to-end and is verified live** against
`claude` 2.1.196 — spawn dialog → mkdir → attach → dictation turn → real reply → `--resume` recall
across reconnects. Real **audio** turns are verified too: a spoken/`jfk.wav` clip → Whisper →
`transcript` → `utterance` → Claude reply, on both the resident GPU whisper server and the CLI
fallback (the shell-out contract is also unit-tested with a fake binary). The **Android app** is
built and verified live on the emulator and the Pixel 8a. (Task-level status — what's built vs.
next — lives in `TODO.md`, not here.)

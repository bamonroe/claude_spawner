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

## Per-session execution target (host vs sandbox)

Status: **implemented** (`internal/session/executor.go`). Goal: let each spawned Claude session run
*either* directly on the host (real host files/toolchains) *or* inside an isolated container sandbox
(disposable, root-inside-the-sandbox) — chosen **per session** via `Session.Target`.

### The single seam

Every turn already funnels through one place: `session.Driver.Turn()` (`internal/session/
session.go`), which `exec`s the `claude` binary in the session's `Dir` and parses its
`stream-json` stdout. Nothing else in the server knows *how* that process is launched. So the whole
feature reduces to making that launch pluggable:

- An **`Executor`** interface (start a `claude` turn given `Dir` + args, return a stdout stream).
  The direct-`exec` is the `host` executor (`HostExecutor`); a `sandbox` executor
  (`SandboxExecutor`) runs the turn inside a container. `Turn()` selects one and is otherwise
  unchanged — the NDJSON parsing, `Setpgid` group-kill, and event fan-out all stay put.
- An **execution-target field** on the `Session` record (`store.go`), set at spawn time and
  persisted in `sessions.json`, so host-vs-sandbox is a durable per-session property the spawn
  dialog chooses. Default = `host`.

## AI backend registry (which AI — orthogonal to where it runs)

Status: **implemented** (`internal/agent`). The `Executor` seam above answers *where* a turn runs
(host / sandbox / SSH). A second, orthogonal seam answers *which AI* runs it and *how* to invoke
and parse it — so the server drives more than `claude`.

- An **`Agent`** (`internal/agent/agent.go`) is a headless backend: an id (persisted on the
  session), a `Bin` (the command to launch), an output `Format`, a `DefaultModel`, a catalogue of
  selectable `Models` (each by a short spoken alias — `opus`/`sonnet`/`fable`, or Codex's presets),
  and a per-backend **arg builder** (`Agent.Args(TurnSpec)`) that emits that backend's exact command
  line. The `Registry` holds the known agents; an empty/unknown id resolves to the default (Claude),
  so records predating the field just run on Claude.
- `Session` gains a durable **`Agent`** (backend id) and **`Model`** (alias). `Driver.Turn` resolves
  the agent, asks it to build the args, passes the resolved backend binary to the `Executor`
  (`Driver.binFor` — empty defers to the executor's own `SPAWNER_*_CLAUDE_BIN`, keeping Claude
  unchanged), and dispatches the output parser on the agent's `Format`.
- **Backend × target is a matrix, not a special case.** Because *which AI* and *where* are separate
  seams, any backend runs on any target: the arg builder never mentions host/sandbox/SSH, and the
  Executor never mentions claude/codex. Adding a backend touches neither the executors nor the
  gateway.

**Two backends ship today.** *Claude* (`--output-format stream-json`; the server mints the
`session_id` and passes `--session-id`/`--resume`). *Codex* (`codex exec` / `codex exec resume`,
`--json` JSONL): Codex **mints its own** session id (`thread_id`, read from the first output event),
so `Agent.SelfAssignsID` tells `Turn` to capture it from the stream and adopt it as the session id
rather than supplying one. Each `Format` has its own parser (`parseStream` / `parseCodexStream`)
normalizing to the same `(reply, usage)` the rest of the server already consumes. Model availability
can be **plan-dependent** (on a ChatGPT-account Codex, only `gpt-5.5` is `-m`-selectable, so its
alternates are reasoning-effort presets); the registry is the single place that catalogue lives.

### The server runs bare metal (no broker)

The server runs **bare metal** as a single binary, as the ordinary user — so it forks `claude` for
host turns and drives the rootless runtime for sandbox turns **directly**, and enforces the
`SPAWNER_ROOT` jail itself (the last hop before any launch). No component holds host root: the
server is a plain user process and sandboxes use a rootless runtime.

> **Design note — the containerized-server + broker detour (reverted 2026-07-06).** An earlier design
> ran the server in a container and put a small host-side **broker** daemon (`cmd/broker`, dialed
> over a Unix socket) in front of the same `HostExecutor`/`SandboxExecutor` code, so the unprivileged
> container could reach the host without host root. It worked, but bought little: the broker itself
> ran bare metal, and the server never needed root, so the container protected the host from almost
> nothing while adding an IPC hop and a whole wire protocol to maintain. It was folded back into the
> binary. Don't re-introduce *that* (a bespoke Unix-socket broker); the privileged shortcuts — a
> `--privileged` server with `--pid=host` + `nsenter` — were rejected for the same "no component holds
> host root" reason and remain rejected.

> **Containerized, the clean way (SSH-native, 2026-07-08).** With host turns running over **SSH**
> (`SPAWNER_SSH=1`), the server *can* run in a container again — but it reaches the host over standard
> SSH instead of a custom broker: `claude` runs on the host, the container needs no host root and no
> privileged shortcuts, and there is no IPC protocol to maintain. This is the thing the broker detour
> was trying to buy, now bought by SSH. It's optional (the bare-metal binary is still the default) and,
> because execution is over SSH, it can run in parallel with a bare-metal instance for a safe cutover.
> Recipe: `deploy/spawner-container.yml` (host networking so `localhost:22` is the host sshd; home +
> roots mounted at the same paths so discovery/browse read where the host writes). See the Dockerfile
> at `server/Dockerfile`.

### SSH-native execution: the host is a dimension, localhost is just another host

With `SPAWNER_SSH=1`, the `host` target is served by the **`SSHExecutor`** instead of the
direct-`exec` `HostExecutor`: every host turn — the local machine included — runs over SSH. A
per-host `SSHPool` (`internal/session/ssh.go`) dials + authenticates once and keeps the connection
alive, opening a cheap channel per turn. Which machine a session runs on is a durable per-session
field, **`Session.Host`** — orthogonal to the host/sandbox target. The **app owns the host
registry** (Settings → Hosts, persisted server-side as `hosts.json`); `Session.Host` names an entry
there, or a bare hostname the pool dials literally with the `SPAWNER_SSH_*` defaults.

`Session.Host` is **always an explicit name** — there is no implicit "empty means localhost"
default. The loopback machine is the host name **`localhost`** (`session.LocalHost`), handled
exactly like any other SSH host (dialed over loopback SSH with the config defaults). It is **not a
special built-in**: `OpenHostStore` seeds a `localhost` entry into a *fresh* registry so a new
deployment lists it out of the box, but it is an ordinary row — editable and deletable like any
other (once the file exists it never re-seeds, so a delete sticks). The one place a default is
applied is at spawn time (`newSession`): a host-target session with no named host is set to
`localhost` so voice/legacy spawns keep working. Everywhere downstream — the executor, transcript
access (`claudeFS`), discovery — treats a hostless host-target session as a bug: the `SSHExecutor`
returns an error rather than silently running it on the local box. This is what makes a
**remote-only deployment** possible — delete the `localhost` host and the server drives only remote
machines, never touching its own box. (Legacy `sessions.json` records with an empty host are
migrated to `localhost` on load; discovered sessions, found by scanning this machine, are named
`localhost`.)

**What `localhost` means depends on the server's network namespace.** Bare metal, it's the machine
the server runs on. In a container it's the container's own loopback — which has no sshd — *unless*
the container shares the host's network. The `deploy/spawner-container.yml` recipe uses **host
networking** precisely so that `localhost:22` inside the container is the **host's** sshd: the seeded
`localhost` host then drives the host machine, and the mounted home/roots line up with the paths the
host writes. A container *without* host networking can't reach the host as `localhost` — that's a
deployment where you'd delete the `localhost` entry and register the host (and any others) as
explicit remotes instead.

### Sandbox sessions (also without host root)

For `sandbox`-target sessions the container's lifetime is **bound to the session**, not the turn:
the `SandboxExecutor` creates a long-lived container at spawn (`Ensure` → `run -d … sleep
infinity`, named `spawner-sbx-<hex>` from `Session.Container`), each turn runs via `exec -w <dir>`
into it, and it's destroyed when the session is deleted (`Remove` → `rm -f`). So packages
installed and services started in one turn persist to the next — a real environment, not a fresh
box per turn. `Ensure` is idempotent and re-run before every turn, so a container lost to a server
restart or manual `rm` is transparently recreated. Spawn-time `Ensure` is best-effort (logged, not
fatal); a hard runtime failure surfaces on the first turn. Use a **rootless Podman / rootless
Docker** runtime (`SPAWNER_SANDBOX_RUNTIME`) so none of this needs host root — the sandbox gets
root *inside itself* and a disposable FS. Session `Dir` is bind-mounted same-path (so the
transcript's project encoding matches the host); the server's whole `$HOME` is also bind-mounted
**read-write at the same path** by default (`SandboxExecutor.HomeMount`, set from `$HOME`), so
dotfiles, `~/.claude`, and project checkouts are writable inside the sandbox exactly as on the host.
Add anything outside `$HOME` via `SPAWNER_SANDBOX_MOUNTS`. Lifecycle hooks live in the gateway spawn (`ensureSandbox`) and
delete (`removeSandbox`) paths; `Driver.EnsureContainer`/`RemoveContainer` bridge to the executor.
At startup `Driver.ReconcileContainers` sweeps **orphans** — managed containers (matched by the
`spawner-sbx-` name prefix) whose session record no longer exists, e.g. deleted while the server was
down — so they don't accumulate; live sessions' containers are left for `Ensure`-before-turn. The
server drives the runtime (create/exec/remove/list) directly as the user.

**Sandbox on a containerized (SSH-native) server.** A containerized server has no container
runtime of its own, so when `SPAWNER_SSH=1` the `SandboxExecutor` is wired with the same
`SSHPool` and drives **rootless podman on the host over SSH** — every `run`/`exec`/`inspect`/`rm`
runs on `localhost` (the co-located host, over loopback SSH), exactly the way host turns already
do. The exec turn streams over SSH via the shared `SSHPool.Stream`/`streamRemote` helper (the same
cancelable, process-group-killed path as a host turn); lifecycle control goes over `SSHPool.Run`.
Every mount/dir path is a **host** path (session `Dir` and `SPAWNER_SANDBOX_MOUNTS` already are,
since sessions are created against the host filesystem), and `HomeMount` stays the container's own
`$HOME` — which the deployment sets to the host user's home (the compose file mounts `$HOME:$HOME`).
With `SPAWNER_SSH` unset the executor keeps running the runtime as **local** child processes
(bare-metal). This is what lets the `sandbox` target — e.g. a `target: sandbox` session with no
`Session.Host` — run on the containerized server, which otherwise fell back to the host executor and
failed with "no host set".

### Net security posture

No component holds host root: the server is a plain user process and sandboxes use a rootless
runtime. Cost is just the `Executor` seam. See `docs/protocol.md` if a spawn-time target selector
reaches the wire protocol (it may not — the dialog can carry it server-side, like `rename`).

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
    browse.go                   host-scoped directory listing for the New-session picker (listing);
                                  lists the chosen host's FS over SSH from "/" (not the local roots)
    messages.go                 wire message constructors
    *_test.go                   httptest+ws integration (auth, spawn, dictation, ask, stream)
  internal/agent/agent.go       AI backend registry: Agent (id/bin/format/models/arg-builder), Claude + Codex
  internal/session/session.go   headless driver: Driver.Turn (per-agent args + parser), parseStream/parseCodexStream
  internal/session/executor.go  pluggable Executor: HostExecutor (direct exec) + SandboxExecutor (runtime)
  internal/session/store.go     durable session registry (file-backed, atomic writes); Session.Target/Container
  internal/session/discover.go  scan ~/.claude/projects for all Claude sessions (adopt/discover)
  internal/session/transcript.go read/stitch on-disk transcripts for `history` (spans clears)
  internal/command/command.go   utterance -> intent parser + StripWake
  internal/command/registry.go  Command registry (single source of truth) + RegistryJSON
  internal/transcribe/          Transcriber interface: WhisperCPP (CLI) + RemoteWhisper (HTTP)
  internal/projects/projects.go spoken-path fuzzy matching against the spawn roots
  internal/tmux/tmux.go         detect a live interactive `claude` in a pane (ClaudeDirs)
  internal/usage/               per-turn token cost tracking + Estimator (server-global usage %)
  internal/config/config.go     env config + spawn-path validation
  internal/docsync/             drift tests: env vars/wire messages/error codes ↔ docs + CLAUDE.md
  cmd/wsclient/main.go          text client for manual testing; -audio streams a WAV
  cmd/gencommands/main.go       regenerate docs/commands.json from the command registry
  main.go                       server entrypoint (built to a single bare-metal binary)
docker-compose.yml              resident whisper/whisper-fast servers (transcription backend)
/sandbox                        Arch-based sandbox image (Containerfile) for `target: sandbox` sessions (see sandbox/README.md)
/whisper                        Vulkan/CPU Dockerfiles for the resident whisper.cpp server (see whisper/README.md)
/deploy                         server systemd user service + env example + rebuild + claude-log helpers (see deploy/README.md)
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

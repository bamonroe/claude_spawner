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
│  - VAD-gated capture    │                            │  - WebSocket gateway         │
│    (streams speech up)  │ <── transcript / output ── │  - Whisper transcription     │
│  - audio capture        │                            │  - wake match + command FSM  │
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

- **Wake word**: matched **server-side, in the transcript** (`command.StripWake`) — there is no
  on-device keyword engine. The phone streams VAD-gated speech up; the server transcribes it and
  looks for the wake phrase (plus its mishearing variants and any custom wake tokens).
- **Transcription (STT)**: server-side Whisper (whisper.cpp or a local Whisper service). The app
  streams captured (VAD-gated) audio; the server returns a transcript and applies the wake/command
  grammar to it. **Repetition-loop guard** (`internal/transcribe`): Whisper hallucinates by
  looping a phrase on long/low-energy clips ("X. X. X. …"). Two mitigations, both in `transcribe`:
  the decoder runs with **no-context** (CLI `-nc`; remote `no_context=true`) so a window can't seed
  the next with its own hallucinated tail, and `clean()` runs `collapseRepeats()`, which drops
  back-to-back duplicate sentences and 3+ repeats of a short phrase before the text hits the
  wake/command seam.
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
  The direct-`exec` `HostExecutor` is now **test-only** — in production the `host` target uses
  `SSHExecutor` (SSH-native execution is unconditional; see below). A `sandbox` executor
  (`SandboxExecutor`) runs the turn inside a container. `Turn()` selects one and is otherwise
  unchanged — the NDJSON parsing, `Setpgid` group-kill, and event fan-out all stay put.
- An **execution-target field** on the `Session` record (`store.go`), set at spawn time and
  persisted in `sessions.json`, so host-vs-sandbox is a durable per-session property the spawn
  dialog chooses. Default = `host`.

## AI backend registry (which AI — orthogonal to where it runs)

Status: **implemented** (`internal/agent`). The `Executor` seam above answers *where* a turn runs
(host / sandbox / SSH). A second, orthogonal seam answers *which AI* runs it and *how* to invoke
and parse it — so the server drives more than `claude`.

- An **`Agent`** (`internal/agent`) is a **self-contained** headless backend, one file per backend
  (`claude.go`, `codex.go`): an id (persisted on the session), a `Bin` (the command to launch), a
  `DefaultModel`, a catalogue of selectable `Models` (each by a short spoken alias —
  `opus`/`sonnet`/`fable`, or Codex's presets). The `Models` slice is the compiled
  **fallback**: a backend may also declare **live discovery** (`DiscoverArgs` + `ParseModels`) —
  a command whose stdout lists the models it can *currently* run — and when a probe succeeds the
  discovered catalogue **shadows** `Models` everywhere it's read (`Agent.Catalog`, guarded for the
  runtime refresh). So a backend fronting an external model store (opencode → Ollama) reports its
  real list with no rebuild, while backends with a fixed set (Claude, Codex) just carry `Models`. It
  also has a per-backend **arg builder**
  (`Agent.Args(TurnSpec)`) that emits that backend's exact command line, its own **stream parser**
  (`Agent.ParseTurn`, normalizing the backend's output to the shared `TurnResult` — reply, usage,
  self-assigned session id), and a declared **transcript layout** (`Agent.Transcript`). The
  backend-neutral turn vocabulary (`ToolUse`/`Usage`/`RateLimit`, `TurnCallbacks`, `TurnResult`)
  lives in `agent/turn.go`. The `Registry` holds the known agents; an empty/unknown id resolves to
  the default (Claude), so records predating the field just run on Claude.
- `Session` gains a durable **`Agent`** (backend id) and **`Model`** (alias). `Driver.Turn` resolves
  the agent, asks it to build the args, passes the resolved backend binary to the `Executor`
  (`Driver.binFor` — empty defers to the executor's own `SPAWNER_*_CLAUDE_BIN`, keeping Claude
  unchanged), and hands the stream to the agent's own `ParseTurn`. **`Turn` contains no per-backend
  branching** — the only conditionals are on declarative Agent fields (`SelfAssignsID`,
  `Transcript`), never on which backend it is.
- **Backend × target is a matrix, not a special case.** Because *which AI* and *where* are separate
  seams, any backend runs on any target: the arg builder never mentions host/sandbox/SSH, and the
  Executor never mentions claude/codex. Adding a backend touches neither the executors nor the
  gateway.

**Three backends ship today.** *Claude* (`--output-format stream-json`; the server mints the
`session_id` and passes `--session-id`/`--resume`). *Codex* (`codex exec` / `codex exec resume`,
`--json` JSONL): Codex **mints its own** session id (`thread_id`, read from the first output event),
so `Agent.SelfAssignsID` tells `Turn` to adopt the id `ParseTurn` returns in
`TurnResult.SessionID` rather than supplying one. Model availability
can be **plan-dependent** (on a ChatGPT-account Codex, only `gpt-5.5` is `-m`-selectable, so its
alternates are reasoning-effort presets); the registry is the single place that catalogue lives.
*opencode* (`opencode run` / `run -s <id>`, `--format json` JSONL) drives **local Ollama** models:
like Codex it **self-assigns** its session id (a `ses_…` id on every event), its models are the
`ollama/*` catalogue served by the provider block in the host user's `~/.config/opencode/opencode.jsonc`
(pointed at the local Ollama server), and `--auto` is its skip-permissions equivalent. It is also
the first backend to use **live model discovery**: `DiscoverArgs` runs `opencode models ollama` on
the host and `ParseModels` turns each `ollama/<id>` line into a model, so whatever opencode is
configured to run appears in the app automatically — pulling a new Ollama model and wiring it into
`opencode.jsonc` (both the user's job — the server treats opencode as the source of truth for what's
runnable) is all it takes, no server rebuild. `Driver.RefreshModels` runs the probe over the SSH
pool at boot (before the provider overlay is validated) and, throttled, on each client connect. It
persists
sessions in a SQLite DB rather than flat files, so its transcript reader shells out to opencode's own
commands (see below).

**Reattach replays each backend's own on-disk transcript.** A session has no live process, so the
`history` page and the on-attach context badge are rebuilt from disk — and *where* that record lives
and *how* it's shaped differs by backend, so the reader is chosen by the agent's declared
`Transcript` layout (`Driver.transcriptReaderFor`). Claude writes
`~/.claude/projects/*/<session_id>.jsonl` (read by `claudeFS`); Codex writes a **rollout** JSONL at
`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl` in an unrelated schema — conversation
prose as `event_msg` `user_message`/`agent_message` lines, context size as `token_count` lines — read
by `codexFS` (`internal/session/codex_transcript.go`). opencode keeps sessions in a **SQLite database**
(`~/.local/share/opencode/opencode.db`), not files, so `opencodeFS`
(`internal/session/opencode_transcript.go`) instead shells out to opencode's own stable commands over
the same SSH seam — `opencode export <id>` for history (mapping its message/part JSON, taking each
turn's context size from the last `step-finish` part's tokens, since the session-level `info.tokens`
is summed across turns) and `opencode session delete <id>` for removal. All three normalize to the same
`[]Message` / `ContextSnapshot` the gateway already sends, so a Codex or opencode session's past turns
replay on reattach exactly like a Claude session's. (These persisted records are *not* the live
`--json` streams the agents' `ParseTurn` consume during a turn.)

### Adding an AI backend (e.g. Gemini CLI, a local model)

The checklist, in dependency order — the design goal is that a new backend is **one new file plus
wiring**, and nothing in the gateway, executors, or clients changes:

1. **`internal/agent/<backend>.go`** — the whole backend in one file, `claude.go` as the template:
   a constructor returning an `*Agent` (id, name, `Bin`, models + default, `SelfAssignsID`,
   `Transcript`), its `build` func (the exact CLI for first-turn / resume / bypass / model), and
   its `ParseTurn` (stream → `TurnResult`, fanning live events out via `TurnCallbacks`). Add parser
   tests beside it (`parse_test.go` has the pattern, with real captured event shapes).
   - **Optional — live model discovery.** If the backend fronts an external, user-managed model set
     (like opencode → Ollama) rather than a fixed catalogue, declare `DiscoverArgs` (the argv whose
     stdout lists runnable models) and `ParseModels` (stdout → `[]Model`). `Driver.RefreshModels`
     runs it over the host SSH pool and installs the result via `Agent.SetDiscovered`; `Models` stays
     as the fallback when a probe fails. Keep discovered aliases in the same scheme as the fallback
     so a stored provider-overlay default/voice choice survives either path.
2. **Register it** in `agent.Default()`.
3. **Transcript reader** — if the backend's on-disk history layout isn't Claude-shaped, add a
   `TranscriptKind` constant and a reader in `internal/session` (see `codex_transcript.go`), and
   teach `transcriptReaderFor` the new kind. If it never persists transcripts, declare
   `TranscriptClaude` and reattach simply replays nothing.
4. **Binaries per target** — env vars in `internal/config` (host + sandbox, following
   `SPAWNER_SSH_CODEX_BIN` / `SPAWNER_SANDBOX_CODEX_BIN`), wired into `Driver.AgentBins` in
   `main.go`. Document them in `CLAUDE.md` (the docsync test enforces this).
5. **Voice spawn vocabulary** — add the backend's spoken name to `spawnAgentWords` in
   `internal/command` so "spawn a <backend> session" works (the visual picker needs nothing: the
   `agents` message advertises the registry dynamically).
6. **Docs** — update the backend list here; `docs/protocol.md` and the clients need no changes.

### The server runs in a container, driving the host over SSH (no broker)

The server runs in a **Docker container** that builds the Go binary from source — the one supported
deployment. It runs as the ordinary user (never host root) and reaches the host over **SSH**
(unconditional — SSH-native is not a toggle): it runs `claude` for host turns and drives the
rootless runtime for sandbox turns **on the host** over that same SSH connection, and reads every
session's Claude transcript back over it. There is no spawn-directory jail — a session may launch
anywhere on the target host. No component holds host root: the server is an unprivileged
container and sandboxes use a rootless runtime on the host. Recipe: the root `docker-compose.yml`
(the `spawner-server` service alongside `whisper`, so one `docker compose up -d --build` launches the
whole backend; host networking so `localhost:22` is the host sshd; only durable state and the
whisper models dir are mounted — discovery, browse, and transcript reads all run on the host over
SSH, not off a host home/root mount). See the Dockerfile at `server/Dockerfile`.

> **Design note — the containerized-server + broker detour (reverted 2026-07-06).** An earlier design
> ran the server in a container and put a small host-side **broker** daemon (`cmd/broker`, dialed
> over a Unix socket) in front of the same `HostExecutor`/`SandboxExecutor` code, so the unprivileged
> container could reach the host without host root. It worked, but bought little: the broker itself
> ran bare metal, and the server never needed root, so the container protected the host from almost
> nothing while adding an IPC hop and a whole wire protocol to maintain. Don't re-introduce *that* (a
> bespoke Unix-socket broker); the privileged shortcuts — a `--privileged` server with `--pid=host` +
> `nsenter` — were rejected for the same "no component holds host root" reason and remain rejected.
> The container reaches the host over **standard SSH** instead (2026-07-08): `claude` runs on the
> host, no host root, no privileged shortcuts, no IPC protocol to maintain — the thing the broker
> detour was trying to buy, now bought by SSH. (There was a bare-metal-binary interregnum between the
> revert and the SSH-native container; it's gone now — the container is the only route.)

### SSH-native execution: the host is a dimension, localhost is just another host

The `host` target is served by the **`SSHExecutor`**: every host turn — the local machine
included — runs over SSH (SSH-native is unconditional; the direct-`exec` `HostExecutor` survives
only as the hermetic unit-test executor, never in the running server). A
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

**What `localhost` means depends on the server's network namespace.** In a container it's the
container's own loopback — which has no sshd — *unless* the container shares the host's network. The
`spawner-server` service in the root `docker-compose.yml` uses **host networking** precisely so that
`localhost:22` inside the container is the **host's** sshd: the seeded
`localhost` host then drives the host machine over SSH (there is no host home/root mount — all of it,
including transcript reads and discovery, goes over that SSH connection). A container *without* host
networking can't reach the host as `localhost` — that's a
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
transcript's project encoding matches the host); the host user's `$HOME` is also bind-mounted into
the **sandbox container** **read-write at the same path** by default (`SandboxExecutor.HomeMount`,
set from `$HOME`), so dotfiles, `~/.claude`, `~/.codex`, and project checkouts are writable inside
the sandbox exactly as on the host.
Add anything outside `$HOME` via `SPAWNER_SANDBOX_MOUNTS`. Lifecycle hooks live in the gateway spawn (`ensureSandbox`) and
delete (`removeSandbox`) paths; `Driver.EnsureContainer`/`RemoveContainer` bridge to the executor.
At startup `Driver.ReconcileContainers` sweeps **orphans** — managed containers (matched by the
`spawner-sbx-` name prefix) whose session record no longer exists, e.g. deleted while the server was
down — so they don't accumulate; live sessions' containers are left for `Ensure`-before-turn. The
server drives the runtime (create/exec/remove/list) directly as the user.

**Sandbox on a containerized (SSH-native) server.** A containerized server has no container
runtime of its own, so the `SandboxExecutor` is wired with the same
`SSHPool` and drives **rootless podman on the host over SSH** — every `run`/`exec`/`inspect`/`rm`
runs on `localhost` (the co-located host, over loopback SSH), exactly the way host turns already
do. The exec turn streams over SSH via the shared `SSHPool.Stream`/`streamRemote` helper (the same
cancelable, process-group-killed path as a host turn); lifecycle control goes over `SSHPool.Run`.
Every mount/dir path is a **host** path (session `Dir` and `SPAWNER_SANDBOX_MOUNTS` already are,
since sessions are created against the host filesystem), and `HomeMount` (`-v $HOME:$HOME`, run by
podman **on the host**) makes the sandbox write its transcript into the host user's `~/.claude`.
The server then reads that transcript — and runs discovery — **over SSH on `localhost`**, not off
its own filesystem: a `sandbox` session carries no `Session.Host`, and `claudeFSFor("")` maps that
empty host to the loopback host and returns the SSH-backed `claudeFS`. Nothing about the sandbox
touches the server container's own `/data` or `$HOME`, which is why those bind mounts are gone from
`docker-compose.yml` (only `state` and the whisper models dir remain).

The `SandboxExecutor`'s local-child-process path (`Pool` nil) survives only for unit tests; the
running server always wires the pool. This is what lets the `sandbox` target — e.g. a
`target: sandbox` session with no `Session.Host` — run on the containerized server, which
otherwise fell back to the host executor and failed with "no host set".

### Net security posture

No component holds host root: the server is a plain user process and sandboxes use a rootless
runtime. Cost is just the `Executor` seam. See `docs/protocol.md` if a spawn-time target selector
reaches the wire protocol (it may not — the dialog can carry it server-side, like `rename`).

## Detached background jobs (survive the turn boundary)

A turn is one short-lived headless `claude` process, so a command that must outlive it can't ride
Claude's in-process `run_in_background`. The `spawner-job` wrapper (embedded via `go:embed` in
`internal/session/bgjob`, staged to each target on demand) launches the command with its **own**
`setsid`/`nohup`, stdin `/dev/null`, and output to a log — so neither the SSH `kill -pgid` teardown
nor the host executor's group-SIGKILL can reach it. Jobs are recorded in an on-target registry
**keyed by working dir** (stable across `clear`/`compress` session-id rotation), the source of
truth; `Session.Jobs`/`PendingNotes` are the persisted mirror.

`Driver.RunOnTarget` runs short commands on the session's *same* target (host fork / `SSHPool.Run` /
`podman exec`), which the gateway's `reconcileJobs` uses at each turn boundary and on attach to poll
the registry. A newly-finished job's bounded output becomes a framed `PendingNotes` entry that
`dictate` prepends to the next turn's prompt (so Claude is told), and `JobsPrimed` gates a one-per-
context instruction telling Claude to use the wrapper. Reconcile/stage errors are swallowed so they
never block a turn. Caveat: sandbox jobs live only as long as the container.

Enforcement (not just priming): the turn injects a Claude **PreToolUse hook** via `--settings`
(`HookSettingsJSON` → `TurnSpec.SettingsJSON` → the Claude agent's argv) whose `Bash` matcher runs
`spawner-job hook`. On a `run_in_background` launch that subcommand emits a PreToolUse `updatedInput`
that **transparently rewrites** the call to `spawner-job start '<original cmd>'` (jq `@sh` quotes the
command; `run_in_background` is cleared) — no cancellation, the same Bash tool just runs the wrapped
command. Fallbacks preserve enforcement: no jq → exit 2 to block with a redirect; unstaged wrapper →
the hook is a graceful no-op. Hooks fire under `--dangerously-skip-permissions`, so it's a hard gate.

## Transcription (internal/transcribe)

The gateway depends only on the `Transcriber` interface; there are **two implementations** and
either can back it:

- **`RemoteWhisper`** (`remote.go`) — POSTs the WAV to a **resident whisper.cpp HTTP server**
  (`/inference`). This is the preferred path on this host, which has an **Nvidia GPU**: the
  `whisper` compose service runs whisper.cpp built with **CUDA** and keeps the model warm
  (`medium.en`, `:8571`), handling both real dictation and the live hands-free draft +
  end-token detection. An optional second, fast draft server (`base.en`, `:8572`) can offload
  the cheap high-frequency work so it never blocks the accurate model — see `whisper/README.md`
  for how to add it. Enabled via `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL`.
- **`WhisperCPP`** (`transcribe.go`) — shells out to the **whisper.cpp CLI** (one process per
  utterance), `exec`'d like `claude`/`tmux`, no server. The fallback when no whisper URL is set.
  It size-picks a model per clip (tiny/base/small) from `SPAWNER_WHISPER_MODEL{,_FAST,_BASE}`.

Opus clips are decoded to 16 kHz mono WAV with **ffmpeg** first (whisper can't read Opus). STT is
disabled unless a model/URL is configured; when disabled the audio path returns `not_implemented`
but text utterances still work. Swapping to faster-whisper or a cloud API (e.g. Groq
large-v3-turbo) stays a one-file change behind the `Transcriber` interface.

Whisper hallucinates on silence (it fills quiet stretches with looped YouTube-outro phrases), so
the resident server images run with **Silero VAD + non-speech-token suppression** as entrypoint
defaults — see `whisper/README.md` (the anti-hallucination defaults) for the details.

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
  internal/agent/               AI backend registry: Agent type + Registry (agent.go), shared turn vocabulary (turn.go), one self-contained file per backend (claude.go, codex.go, opencode.go)
  internal/session/session.go   headless driver: Driver.Turn (per-agent args + parser), parseStream/parseCodexStream
  internal/session/executor.go  pluggable Executor: HostExecutor (direct exec) + SandboxExecutor (runtime)
  internal/session/store.go     durable session registry (file-backed, atomic writes); Session.Target/Container
  internal/session/settings.go  server-global preferences persisted to settings.json (survives restart; e.g. resident whisper model)
  internal/session/discover.go  scan ~/.claude/projects for all Claude sessions (adopt/discover)
  internal/session/transcript.go read/stitch Claude on-disk transcripts for `history` (spans clears)
  internal/session/codex_transcript.go  codexFS: read Codex rollout files for `history`/context badge
  internal/session/opencode_transcript.go  opencodeFS: `opencode export`/`session delete` for `history`/context badge
  internal/command/command.go   utterance -> intent parser + StripWake
  internal/command/registry.go  Command registry (single source of truth) + RegistryJSON
  internal/transcribe/          Transcriber interface: WhisperCPP (CLI) + RemoteWhisper (HTTP)
  internal/projects/projects.go spoken-path term tokenizing + fuzzy ranking (Terms/Rank) for the resolver
  internal/tmux/tmux.go         detect a live interactive `claude` in a pane (ClaudeDirs)
  internal/usage/               per-turn token cost tracking + Estimator (server-global usage %)
  internal/config/config.go     env config + spawn-path validation
  internal/docsync/             drift tests: env vars/wire messages/error codes ↔ docs + CLAUDE.md
  cmd/wsclient/main.go          text client for manual testing; -audio streams a WAV
  cmd/gencommands/main.go       regenerate docs/commands.json from the command registry
  main.go                       server entrypoint (built into the Docker image from server/Dockerfile)
docker-compose.yml              the whole stack: spawner-server gateway + whisper transcription (one `up` launches both)
/sandbox                        Arch-based sandbox image (Containerfile) for `target: sandbox` sessions (see sandbox/README.md)
/whisper                        Vulkan/CPU Dockerfiles for the resident whisper.cpp server (see whisper/README.md)
/deploy                         containerized server compose + env example + container-rebuild + claude-log helpers (see deploy/README.md)
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

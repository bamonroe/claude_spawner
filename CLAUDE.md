# CLAUDE.md

Guidance for Claude Code instances working in this repository.

## Start here: the documentation map

This repo keeps documentation **de-duplicated** — every fact has exactly **one** authoritative
home. When you need to know or change something, go to its owner below; don't restate a fact in a
second file (link to the owner instead). This table is itself the index: read it first.

| You want to know / change…                    | Authoritative home                          | Enforced by |
|-----------------------------------------------|---------------------------------------------|-------------|
| **What to do next / what's done** (task state)| `TODO.md`                                   | discipline (the `TODO.md` rule below) |
| **How to work here** (conventions, decisions, rules) | `CLAUDE.md` (this file)               | discipline |
| **How the system works internally** (data flow, session driver, transcription, repo layout) | `docs/architecture.md` | discipline |
| **How a user runs/uses it** (setup, build & run, security, phase history) | `README.md`         | discipline |
| **WebSocket wire protocol** (every message + error code) | `docs/protocol.md`               | `internal/docsync` tests |
| **"hey buddy" command grammar** + how to add a command | `docs/commands.md` (prose) + `command.Registry` (code) → `docs/commands.json` (generated) | `internal/command` + `cmd/gencommands` |
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
   **one** place only — status/tasks in `TODO.md`, architecture in `docs/architecture.md`,
   conventions here, run/history in `README.md` —
   and the update rules below (and in `README.md`) keep that single copy current.

## What this project is

**claude_spawner** is a voice-driven remote control for Claude Code. It has two halves:

1. **Android app** (Kotlin) — listens for the wake word **"hey buddy"**, captures voice,
   and acts as a passthrough terminal to remote Claude Code sessions.
2. **Server** (Go) — runs on the user's machine, spawns and manages **Claude Code sessions**
   (driven headless), and bridges voice/text between the app and those sessions.

The user speaks; the app transcribes (via server-side Whisper); the text is either interpreted
as a **reserved control command** or passed through to the currently attached Claude Code
session. Claude Code's output is streamed back to the phone and read aloud (TTS).

**The tool is self-hosting.** The user develops claude_spawner *through* claude_spawner — the
Pixel 8a runs the app attached to the very Claude Code session doing the work, so a build you ship
becomes the client you're talking to. Expect to see your own messages appear in the app's chat log,
and remember that an Android change you push can affect the live client mid-conversation.

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

Reserved commands live server-side as a parseable grammar (see `docs/commands.md`).
The wake word is detected **on-device** (Porcupine); everything after it is streamed to the
server for transcription and parsing. Keep the wake word and the command vocabulary in **one
authoritative place** so the app and server agree.

## Architecture — see `docs/architecture.md`

The internals live in **`docs/architecture.md`**: the data-flow diagram, the resolved decision to
drive `claude` **headless** in `stream-json` mode (a session is a `session_id` on disk, **we do not
scrape the TUI** — don't re-litigate this), how tmux is inspected only to detect a live interactive
`claude`, the transcription implementations, the text seam, and the repository layout. Read it when
you're changing the data path, the session driver, or STT. Two load-bearing rules from it:

- **Don't scrape the TUI.** Responses are captured headless via `--output-format stream-json`; the
  clean `result` event is the text we speak. This was validated end-to-end; keep it.
- **One active writer per session at a time.** Don't run a headless turn against a `session_id` a
  human is editing live in a terminal.

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

## Build, run & repository layout — see `docs/architecture.md` and `README.md`

The **repository layout** (every package and what it does) and the internals are in
`docs/architecture.md`. **How to build and run** the server (a bare-metal single binary under a
systemd user service) is in `README.md`. Don't restate either here.

## Config env vars

All read in `internal/config`; the `docsync` drift test requires each to appear here, backticked:

- `SPAWNER_ADDR` (`:8080`), `SPAWNER_TOKEN` (**required**), `SPAWNER_ROOT` (colon-separated
  spawn-dir jail), `SPAWNER_STATE` (`sessions.json`), `SPAWNER_CLAUDE_BIN` (`claude`).
- Transport TLS (all optional; empty = plain `ws://`, fine behind Tailscale): `SPAWNER_TLS_CERT`
  and `SPAWNER_TLS_KEY` (PEM cert/key — set **both** to serve `wss://`; one without the other is a
  startup error), `SPAWNER_TLS_CLIENT_CA` (PEM CA bundle — when set, the app must present a client
  certificate signed by one of these CAs **in addition to** the token → mutual TLS; requires the
  cert/key pair).
- CLI STT: `SPAWNER_WHISPER_BIN` (`whisper-cli`), `SPAWNER_WHISPER_MODEL` (path; enables STT),
  `SPAWNER_WHISPER_MODEL_FAST` / `SPAWNER_WHISPER_MODEL_BASE` (per-size model paths for the
  clip-length model picker), `SPAWNER_WHISPER_LANG` (`en`), `SPAWNER_FFMPEG_BIN` (`ffmpeg`).
- Resident-server STT: `SPAWNER_WHISPER_URL` (accurate server), `SPAWNER_WHISPER_FAST_URL` (fast
  draft/detection server), `SPAWNER_WHISPER_MODEL_NAME` (`medium.en`; reported to clients),
  `SPAWNER_WHISPER_FAST_MAX_SEC` (`2.5`; clips shorter than this use the fast server).
- Sandbox sessions (per-session `target: sandbox` execution): `SPAWNER_SANDBOX_IMAGE` (container
  image; **empty disables** the sandbox target), `SPAWNER_SANDBOX_RUNTIME` (`podman`; the container
  CLI — rootless so no host root), `SPAWNER_SANDBOX_CLAUDE_BIN` (`claude`; the binary inside the
  image), `SPAWNER_SANDBOX_MOUNTS` (comma-separated extra `-v` specs, e.g. sharing `$HOME/.claude`),
  `SPAWNER_SANDBOX_RUN_ARGS` (space-separated extra `run` flags, e.g. `--userns=keep-id`).
- SSH-native execution (host turns over SSH; **empty `SPAWNER_SSH` keeps the direct-fork host
  path** — transitional until loopback SSH is verified): `SPAWNER_SSH` (`1` enables; then every
  host-target turn, local included, runs over SSH with no special-cased localhost fork),
  `SPAWNER_SSH_USER` (login user; empty = current OS user), `SPAWNER_SSH_PORT` (`22`),
  `SPAWNER_SSH_KEY` (private-key path; empty relies on the `ssh-agent`), `SPAWNER_SSH_KNOWN_HOSTS`
  (`~/.ssh/known_hosts`; host keys are always verified — no insecure mode), `SPAWNER_SSH_CLAUDE_BIN`
  (`claude`; the remote binary).
- Restart: `SPAWNER_RESTART_CMD` — a shell command (run via `sh -c`, detached) fired by the app's
  restart button; empty disables restart. The server runs bare metal (a single binary, not
  containerized), so it forks `claude` for host turns and drives the rootless runtime for sandbox
  turns itself — there is no separate host broker. The command is fired in its own process group and
  the systemd unit uses `KillMode=process`, so it survives the server's own teardown. The deployment
  points it at just `systemctl --user restart --no-block spawner-server` (the button only bounces the
  service, relaunching the current binary); rebuilding + deploying new code is a separate manual step
  (`deploy/rebuild.sh`, which rebuilds the binary then restarts the unit).

## Token discipline — keep the context small

Context tokens are the main cost here, so default to the frugal path:

- **Read in slices, not whole files.** Reach for `grep`/`glob` to find the target, then `Read` with
  `offset`/`limit` around it. Only read a whole file when you genuinely need all of it. Never re-read
  a file you just edited — the edit already confirmed the new state.
- **Delegate broad searches to `Explore` subagents.** Anything that means sweeping many files or
  directories to answer a "where/how is X done" question goes to an `Explore` (or `general-purpose`)
  subagent, which reads the files in its own context and returns just the conclusion — the file
  dumps never land in this conversation. Do the search inline only when it's one or two known files.
- **Don't restate; link.** This repo is de-duplicated for the same reason — point at the owning doc
  (per the map above) instead of pasting its content into a reply or a new file.
- **Prefer targeted output.** Pipe long command output through `head`/`tail`/`grep`; don't cat whole
  logs or list huge trees. Ask for the smallest thing that answers the question.
- **Phone/voice replies stay short.** In phone/concise mode, suppress code blocks, diffs, and long
  paths — summarize in spoken sentences (that's both the UX and a token win).

## Conventions

- Keep the **command grammar** and the **WebSocket message protocol** in `/docs` as the single
  source of truth; both client and server reference it.
- **Adding or changing a "hey buddy" command** follows a checked Registry→JSON→APK pipeline — the
  full procedure is in `docs/commands.md` ("Adding or changing a command"). You never hand-edit the
  app's command list.
- Server: idiomatic Go, `gofmt`, errors wrapped with context. Keep tmux interaction behind one
  package so the shell-out details are isolated and testable.
- Android: Kotlin, keep audio/wake-word, networking, and UI in separate modules/packages.
- **Promote stable builds to the phone.** Iterate on the Dockerized emulator (fast, disposable),
  but once a feature is shown to be quite stable there, also install the APK on the physical
  **Pixel 8a** over adb so it's running on real hardware. The two adb worlds and the exact install
  commands are in the `android-dev` skill (its home is `/data/android`, where it's a
  directory-scoped skill); the emulator is for iteration, the phone is where a settled feature lands.
  - **Finish Android work by installing on the phone.** The Pixel 8a is the live self-hosting
    client, so a shippable APK isn't "done" until it's on the phone — the last step of any Android
    change is `adb -s <phone> install -r` (see the `android-dev` skill). This is doubly required
    when the phone is where the feature's *final* verification has to happen (anything the emulator
    can't validate — real mic/hands-free, real turns, hardware) or when the emulator run left the
    feature only partly checked: don't stop at the emulator and leave the phone on the old build.
    Install it before reporting the work complete.
- When you change the architecture or make a design decision (e.g. the headless-vs-TUI capture
  question in `docs/architecture.md`), record it in the owning doc and the README so it isn't
  re-litigated.

### Git: commit atomically, at will and frequently — and push freely

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
- **Push freely and liberally.** You have standing authorization to `git push` to `origin` without
  asking first — don't let local commits sit unpushed. Push after committing (or after a short run
  of related commits); keeping the remote current is part of "done," same as committing.

### Document every feature immediately, in the same breath as writing it

**A feature isn't done until it's documented.** Write the documentation *during* the feature work,
or immediately after — never defer it to "later," and never ship code without it.

- Every new feature gets full user-facing documentation in `README.md` as part of the same work.
- Keep the single-source-of-truth docs in sync in the same pass: a new voice command goes in
  `docs/commands.md`, a new WebSocket message goes in `docs/protocol.md`.
- Docs land in the same commit as the feature (or an immediately-following commit) — a feature
  commit with no accompanying documentation is incomplete.

## `TODO.md` is the live task list — keep it current

`TODO.md` (repo root) is the single source of truth for active and completed work — status
("what's built, what's next") lives there only, not here or in `README.md` (both link to it). The
historical phase roadmap is separate, in `README.md`. **Update `TODO.md` in the same commit that
changes the work it describes:** add proposed features/tests unchecked; check off and date
completed ones; remove dropped ones with a one-line why. A stale `TODO.md` means the change isn't
done — same rule as the docs.

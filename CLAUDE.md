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
| **How to develop the web client** (wasmJs source sets, `js()` interop idiom, iterate loop) | `docs/web-client.md` | `internal/docsync` client↔server wire tests |
| **Config env vars** (`SPAWNER_*`)             | `CLAUDE.md` (config section) — code owns them in `internal/config` | `internal/docsync` tests |

**Two classes of fact, two ways they're kept honest:**

1. **Code-derived facts** (env vars, wire messages, error codes, the command list) are owned by
   the code. The docs are a mirror, and a **drift test fails the build** if they fall out of sync:
   - `internal/command` ↔ `docs/commands.json` (regenerate with `go run ./cmd/gencommands`);
   - `internal/docsync` ↔ `docs/protocol.md` + `CLAUDE.md` (env vars, in/outbound messages **and
     their payload field names**, error codes) — see that package's doc comment. It also cross-checks the **Kotlin client's** wire
     strings (`net/Protocol.kt` — message types both directions, audio codecs) against the Go
     gateway (`clientsync_test.go`), so a message added on one side without the other fails the
     build; deliberately one-sided messages live in the tests' exemption maps with reasons. A red `go test ./...` names exactly what's stale.
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
The wake word is detected **server-side, in the transcript** (`command.StripWake`) — there is no
on-device keyword engine. The app streams VAD-gated speech to the server, which transcribes it
(Whisper) and matches the wake word and command vocabulary. Keep the wake word and the command
vocabulary in **one authoritative place** so the app and server agree.

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
- There is **no spawn-directory jail** — a session may spawn **anywhere** on the target host, by
  voice or via the visual picker. The voice dialog takes a full spoken path and fuzzy-resolves each
  segment against the target's real filesystem over SSH; the visual "new session" picker browses the
  chosen host's whole filesystem over SSH (starting at `/`). The user opted into this, and given the
  server is already trusted and Claude runs with permissions skipped, it's consistent — the whole
  surface stays behind the authenticated WebSocket, which is what actually gates access.

## Build, run & repository layout — see `docs/architecture.md` and `README.md`

The **repository layout** (every package and what it does) and the internals are in
`docs/architecture.md`. **How to build and run** the server (a Docker container that builds the Go
binary and drives the host over SSH — the one supported deployment) is in `README.md`. Don't restate
either here.

## Config env vars

All read in `internal/config`; the `docsync` drift test requires each to appear here, backticked:

- `SPAWNER_ADDR` (`:8080`), `SPAWNER_TOKEN` (**required**), `SPAWNER_WEB_DIR` (empty = disabled; a
  directory holding the built Compose/Wasm web-client bundle — `index.html` + `spawnerweb.js` +
  `.wasm` — served as static files at `/` alongside the `/ws` gateway, so one binary hosts both the
  API and the browser client. The static assets are public; the sensitive surface stays behind the
  token-authenticated `/ws` handshake. In the containerized deploy the bundle is **baked into the
  image** at `/srv/web` — `deploy/rebuild-container.sh` stages the Gradle output into the build
  context (building it in a throwaway Gradle container if missing, so a fresh clone's first deploy
  ships the client too), so a `rebuild` ships the current client with no host mount),
  `SPAWNER_STATE` (`sessions.json`), `SPAWNER_PROFILES` (`profiles.json`; optional
  app-managed JSON execution-profile catalogue — the app is the source of truth (like
  `hosts.json`/`identities.json`), the server persists it and re-broadcasts on change. A missing file
  is seeded on first run with starter profiles from the flat sandbox env vars below — `bare-metal`
  (host, marked default), plus `sandbox`/`locked` when `SPAWNER_SANDBOX_IMAGE` is set — then the app
  owns it. "Default" is a per-profile marker the user sets, not a fixed profile),
  `SPAWNER_PROFILE_VARS` (optional JSON object of string values — the server-wide `{{.Vars.X}}`
  substitution set for profile templating, e.g. `{"OllamaHost":"pickle.bam.net"}`. A profile's own
  `vars` overlay these; profile-derived built-ins are `{{.Home}}`, `{{.Session}}`, `{{.Dir}}`.
  Referencing an undefined var fails the turn loudly. Empty = no global vars),
  `SPAWNER_PROVIDERS` (`providers.json`; optional app-managed JSON overlay of
  per-backend (AI-provider) settings — the model a fresh spawn defaults to and which
  models the voice `list models`/`use model N` commands enumerate (Settings →
  Providers). The backends themselves are compile-time; this only stores the user's
  overrides, validated against the live registry. A missing file means no overrides
  (compiled default model, all models voice-enabled). Like the profile/host
  catalogues, the app is the source of truth and the server persists it and
  re-broadcasts the `agents` message on change),
  `SPAWNER_HOSTS` (`hosts.json`; the
  app-managed SSH host registry — the app is the source of truth, this file just persists it),
  `SPAWNER_IDENTITIES` (`identities.json`; the app-managed SSH identity registry — names + public
  keys), `SPAWNER_SSH_KEYS` (`ssh_keys`; directory holding each identity's private key, `0600`; the
  private material never leaves the server, the app only sees/copies the public key),
  `SPAWNER_CLAUDE_BIN` (`claude`; the host binary for Claude-backend sessions — the first entry
  in the AI backend registry, see `docs/architecture.md`. Codex's per-target binaries are
  `SPAWNER_SSH_CODEX_BIN` for host/SSH turns and `SPAWNER_SANDBOX_CODEX_BIN` for the sandbox;
  opencode's are `SPAWNER_SSH_OPENCODE_BIN` (host/SSH, default `opencode`) and
  `SPAWNER_SANDBOX_OPENCODE_BIN` (sandbox, default `opencode`). The opencode backend drives local
  Ollama models — its model catalogue is `ollama/*`, resolved via the provider block in the host
  user's `~/.config/opencode/opencode.jsonc`, which must point at the running Ollama server.
  Antigravity's (Google's Gemini-powered `agy` CLI) per-target binaries are `SPAWNER_SSH_AGY_BIN`
  (host/SSH, default `agy`) and `SPAWNER_SANDBOX_AGY_BIN` (sandbox, default `agy`). Antigravity is
  driven non-interactively via `agy --prompt` — it has no machine-readable stream mode, so only the
  final spoken reply is captured (no live tool events or token accounting), and its caller-supplied
  `--conversation` id makes it resumable like Claude).
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
  `SPAWNER_WHISPER_FAST_MODEL_NAME` (`base.en`; the fast server's boot model, same lifecycle),
  `SPAWNER_WHISPER_MODELS_DIR` (the host directory of ggml model files the whisper containers
  mount at `/models`; when set, its model names are sent to clients as a settings picker —
  empty = free-text entry only),
  `SPAWNER_WHISPER_FAST_MAX_SEC` (`2.5`; clips shorter than this use the fast server).
- Dedicated wake-word / end-token detector (the LiveKit epic, see `TODO.md`): `SPAWNER_WAKEWORD_URL`
  (base URL of the resident `spawner-wakeword` sidecar, e.g. `http://localhost:9060` — the Rust
  service wrapping LiveKit's runtime; it slides a 2s window over each clip and returns peak per-model
  scores, `POST /detect`). When set, live hands-free wake ("bump bump") / end ("beep beep") detection
  scores the dedicated model instead of fast-transcribing the clip and string-matching; empty
  disables it and detection falls back to the Whisper string-match. Accurate commit transcription is
  unaffected either way. `SPAWNER_WAKEWORD_THRESHOLD` (`0.5`; the score in `[0,1]` at/above which a
  token counts as detected — the trained models' optimal point is ~`0.04`–`0.07`, so lowering it
  trades a few false positives for near-zero misses). Detector *models* are trained out-of-tree —
  see the training project at `/data/livekit_training` (the app only consumes a finished model).
- Server-side TTS (the Kokoro epic, see `TODO.md`): `SPAWNER_TTS_URL` (base URL of the resident
  Kokoro-FastAPI server, e.g. `http://localhost:8880` — the `kokoro` compose service; empty
  disables server TTS and clients use on-device speech), `SPAWNER_TTS_VOICE` (`af_heart`; default
  Kokoro voice until a client picks one), `SPAWNER_TTS_FORMAT` (`opus`; synthesis response format:
  mp3 | wav | opus | flac | pcm).
- Sandbox sessions (per-session `target: sandbox` execution): `SPAWNER_SANDBOX_IMAGE` (container
  image; **empty disables** the sandbox target), `SPAWNER_SANDBOX_RUNTIME` (`podman`; the container
  CLI — rootless so no host root), `SPAWNER_SANDBOX_CLAUDE_BIN` (`claude`; the binary inside the
  image), `SPAWNER_SANDBOX_CODEX_BIN` (`codex`; the codex binary inside the image for Codex-backend
  sandbox sessions), `SPAWNER_SANDBOX_MOUNTS` (comma-separated extra `-v` specs, e.g. sharing `$HOME/.claude`),
  `SPAWNER_SANDBOX_RUN_ARGS` (space-separated extra `run` flags, e.g. `--userns=keep-id`).
- SSH-native execution (**unconditional** — every host-target turn, local included, runs over SSH
  with no special-cased localhost fork, and the sandbox's podman + transcript reads run over the
  same pool on the loopback host; the running server never touches its own filesystem for Claude
  state. The direct-fork `HostExecutor` survives only as the hermetic unit-test executor, never in
  production): `SPAWNER_SSH_USER` (login user; empty = current OS user), `SPAWNER_SSH_PORT` (`22`),
  `SPAWNER_SSH_KEY` (private-key path; **empty = the server self-manages its OWN keypair**, minting an
  ed25519 key under the state dir (`<state>/ssh/id_ed25519`) on first boot and writing the public key
  to `<key>.pub` + logging it — install that in the target host's `~/.ssh/authorized_keys` to grant
  access; a set path overrides and is used as-is), `SPAWNER_SSH_KNOWN_HOSTS`
  (`~/.ssh/known_hosts`; host keys are always verified — no insecure mode. The server **owns**
  this file and **auto-seeds** it: the loopback host is trusted on first boot, adding a host in the
  app records its key trust-on-first-use, deleting the host forgets it, and the running pool reloads
  the file so it takes effect without a restart), `SPAWNER_SSH_CLAUDE_BIN`
  (`claude`; the remote claude binary), `SPAWNER_SSH_CODEX_BIN` (`codex`; the remote codex binary for
  Codex-backend SSH sessions — SSH reuses the host target, so this is the host codex binary).
- Restart: `SPAWNER_RESTART_CMD` — a shell command fired by the app's restart button; empty disables
  restart. The server runs in a Docker container that builds the Go binary and drives the host over
  SSH (host `claude` turns and the rootless sandbox runtime both execute on the host — no separate
  host broker). The button does a **rebuild+recreate**: it runs `deploy/rebuild-container.sh`
  detached **on the host**, which must run there because a recreate replaces the very container the
  server runs in — an in-container command would be killed mid-recreate, so `setsid` decouples it.
  The server runs the command on the host over its own Go-native SSH connection pool (no openssh
  client — the container needs no `/etc/passwd` entry). The restart message carries a **`mode`**
  (three buttons in the app): the server substitutes the `%REBUILD%` token in the command with the
  mode and passes it to the script — **`build`** rebuilds the image only and leaves the running
  container in place (the live session isn't bounced; the new image is staged for a later restart),
  **`bounce`** recreates the container from the existing image (fast, no code change, no rebuild),
  and **`rebuild`** (the default, and the voice command) does a `--no-cache` recompile then recreate.
  Commands with no `%REBUILD%` token always rebuild. See `deploy/README.md`.

## Token discipline — keep the context small

Context tokens are the main cost here, so default to the frugal path:

- The user is often interacting through phone speech-to-text. Treat spoken file and directory names
  as approximate: underscores, spelling, and capitalization may be wrong, so check likely matches
  before saying a file is missing.
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
  - **Installing on the phone never interrupts a turn** — `adb install -r` swaps the APK without
    touching the running WebSocket/session. So there's no "good moment" to wait for: whenever a
    phone-side feature is stable and ready to deploy, just push it to the phone. Don't ask first.
- When you change the architecture or make a design decision (e.g. the headless-vs-TUI capture
  question in `docs/architecture.md`), record it in the owning doc and the README so it isn't
  re-litigated.

### Worktrees: parallel app/server development

This repo is checked out as **two git worktrees of the one repository**, so an app-focused agent and
a server-focused agent can work at the same time without colliding on disk:

- **`/data/claude_spawner`** — branch **`master`**, the **server** worktree (Go gateway, session
  driver, docs, deploy). This is the primary/home checkout.
- **`/data/claude_spawner_app`** — branch **`app`**, the **Android app** worktree (Kotlin client,
  UI, settings screens, button layout/appearance).

They share **one git history and one set of tracked files** — every doc (`CLAUDE.md`, `docs/`,
`TODO.md`, `README.md`) exists identically in both trees, so nothing is lost between them; edit a
doc in whichever tree you're in and it merges over. Rules:

- **One agent per worktree.** Don't edit the same tree from two agents; that's the collision the
  split exists to prevent. Stay in your tree's lane — the app worktree does client work, the server
  worktree does server/docs work.
- **The wire protocol is still shared and drift-tested.** A protocol change touches *both* sides
  (`net/Protocol.kt` and the Go gateway), and the `docsync` build test still enforces it across the
  merged history — so a protocol change is the one thing that isn't cleanly parallel: coordinate it,
  land it, and merge before the other tree builds on it.
- **Merge normally, often.** Each branch commits and pushes independently (both push to the same
  `origin`); merge `app` ↔ `master` like any git merge to reconcile. Keep them from drifting far
  apart, especially around shared docs and the protocol.
- Manage worktrees with `git worktree list` / `git worktree add` / `git worktree remove`.

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

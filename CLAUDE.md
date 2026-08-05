# CLAUDE.md

Guidance for Claude Code instances working in this repository.

## Start here: the documentation map

This repo keeps documentation **de-duplicated** — every fact has exactly **one** authoritative
home. When you need to know or change something, go to its owner below; don't restate a fact in a
second file (link to the owner instead). This table is itself the index: read it first.

| You want to know / change…                    | Authoritative home                          | Enforced by |
|-----------------------------------------------|---------------------------------------------|-------------|
| **What to do next** (open/active task state)  | `TODO.toml` (via the `todo` skill)          | `scripts/todo.sh validate` + the rule below |
| **What's finished** (archive of fully-done features/epics) | `FINISHED.toml` (via the `todo` skill) | `scripts/todo.sh validate` + the rule below |
| **How to work here** (conventions, decisions, rules) | `CLAUDE.md` (this file)               | discipline |
| **How the system works internally** (data flow, session driver, transcription, repo layout) | `docs/architecture.md` | discipline |
| **How a user runs/uses it** (setup, build & run, security, phase history) | `README.md`         | discipline |
| **WebSocket wire protocol** (every message + error code) | `docs/protocol.md`               | `internal/docsync` tests |
| **"hey buddy" command grammar** + how to add a command | `docs/commands.md` (prose) + `command.Registry` (code) → `docs/commands.json` (generated) | `internal/command` + `cmd/gencommands` |
| **How to develop the web client** (wasmJs source sets, `js()` interop idiom, iterate loop) | `docs/web-client.md` | `internal/docsync` client↔server wire tests |
| **Config env vars** (`SPAWNER_*`)             | `docs/config.md` — code owns them in `internal/config` | `internal/docsync` tests |

**Two classes of fact, two ways they're kept honest:**

1. **Code-derived facts** (env vars, wire messages, error codes, the command list) are owned by
   the code. The docs are a mirror, and a **drift test fails the build** if they fall out of sync:
   - `internal/command` ↔ `docs/commands.json` (regenerate with `go run ./cmd/gencommands`);
   - `internal/docsync` ↔ `docs/protocol.md` + `docs/config.md` (env vars, in/outbound messages **and
     their payload field names**, error codes) — see that package's doc comment. It also cross-checks the **Kotlin client's** wire
     strings (`net/Protocol.kt` — message types both directions, audio codecs) against the Go
     gateway (`clientsync_test.go`), so a message added on one side without the other fails the
     build; deliberately one-sided messages live in the tests' exemption maps with reasons. A red `go test ./...` names exactly what's stale.
   So: **change the code, then `go test ./...` tells you which doc to update.** Never hand-maintain
   a second copy the tests don't check. (Go caches test results on Go-source inputs, not the
   Markdown files — a code change always re-runs the checks; for a **doc-only** edit run the
   canonical drift check uncached: `go test ./... -count=1`.)
2. **Narrative facts** (status, "verified live", roadmap history) can't be tested, so they live in
   **one** place only — status/tasks in `TODO.toml`, architecture in `docs/architecture.md`,
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

**Do not rebuild/recreate the server while the user is talking through the app.** Running
`deploy/rebuild-container.sh`, using the app restart button, or otherwise recreating
`spawner-server` drops the live WebSocket and kills any in-flight turn, including the session the
user may be using to talk to you. A background job only keeps the command alive; it does not make the
server restart safe. Before starting a server rebuild/recreate, explicitly tell the user it will
interrupt the app connection and wait for them to confirm a safe moment. If you need a rebuild later,
leave the commit pushed and say it is pending instead of starting it silently.

**But building the image only (the `build` mode) IS background-safe — you may do it unprompted.**
The distinction is recreate vs. build: only *recreating* the container (`bounce`/`rebuild`) drops
the live WebSocket. A plain image **build** (`deploy/rebuild-container.sh build`, or the app's
build-only restart mode) recompiles a new image and leaves the running container untouched, so it
never interrupts the in-flight turn. So after you push server changes, you may kick off an
image build in the background on your own — then all the user has to do is tap the fast **restart
container** (`bounce`) button to pick it up, instead of waiting on the slow rebuild-and-recreate.
Prefer this: land the server commits, background-build the image, and tell the user a bounce is all
that's pending.

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

Every `SPAWNER_*` env var lives in **`docs/config.md`** — the authoritative reference, one entry per
var (default, meaning, how it's seeded). Code owns them in `internal/config`; the `internal/docsync`
drift test requires each to appear in `docs/config.md`, backticked. Add a var to the code, then
document it there — don't restate the list here.

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
- **Build and test Android through `/data/android`.** For Android app work, first read the
  `android-dev` skill in `/data/android/.claude/skills/android-dev` and the reference docs in
  `/data/android/CLAUDE.md`. That directory is the single front door for both building and testing:
  build APKs with the disposable Docker builder (`/data/android/build.sh <project-dir> [gradle-task]`)
  instead of relying on a host JDK/SDK/Gradle toolchain, and use the skill scripts for emulator
  install/launch/screenshots/UI dumps and physical-device targeting. Before building an APK that will
  be installed on a physical device, run a clean build (for this app, `:app:clean :app:assembleDebug`)
  or install the exact same APK already tap-tested on the emulator; Kotlin Multiplatform incremental
  builds can otherwise ship stale shared-module dex.
- **Iterate on the emulator; the BAM store handles the phone.** Verify Android changes on the
  Dockerized emulator (fast, disposable) using the `android-dev` skill (its home is
  `/data/android`, where it's a directory-scoped skill). You do **not** install on the physical
  Pixel 8a as a final step — shippable builds now go to the **BAM store** automatically via the
  Android tooling, which is what puts them on the phone. Finish Android work at a clean,
  emulator-verified build; don't hand-push APKs over adb unless the user explicitly asks.
- When you change the architecture or make a design decision (e.g. the headless-vs-TUI capture
  question in `docs/architecture.md`), record it in the owning doc and the README so it isn't
  re-litigated.

### One tree, both halves: server and app live together

This repo is a **single working tree on `master`** (`/data/claude_spawner`) holding **both** the Go
server and the Kotlin Android app. It was briefly split into two parallel git worktrees (a `master`
server tree and an `app` client tree); that experiment was folded back in — the `app` branch is
merged and gone — because coordinating shared docs and the wire protocol across two trees cost more
than the parallelism saved. Work on either half here.

- **The wire protocol spans both sides and is drift-tested.** A protocol change touches *both*
  `net/Protocol.kt` (app) and the Go gateway, and the `docsync` build test enforces they agree —
  change them together in the one tree.
- **App code still builds through `/data/android`**, per the Android convention above — this tree
  holds the Kotlin source; the disposable Docker builder compiles it.

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

## `TODO.toml` is the live task list, `FINISHED.toml` is the archive — keep both current

`TODO.toml` (repo root) is the single source of truth for **open** work — status ("what's next")
lives there only, not here or in `README.md` (both link to it). The historical phase roadmap is
separate, in `README.md`. `FINISHED.toml` is the archive of completed work, newest-first.

Both are **TOML**, not Markdown, and each task carries structured metadata (`id`, `status`,
`category`, `urgency`, `order`, `created`/`completed`, `tags`). **Drive them through the `todo`
skill** — `scripts/todo.sh <command>`, documented in `.claude/skills/todo/SKILL.md` — rather than
hand-editing, so ids, ordering, and metadata stay consistent and diffs stay small. The skill also
answers "how many tasks / bugs are left" (`scripts/todo.sh stats`).

- **Update them in the same commit that changes the work they describe:** add proposed
  features/tests with `scripts/todo.sh add …`; drop descoped ones with
  `scripts/todo.sh remove <id> --reason "…"`.
- **When a task is fully finished** (built, tested, documented), run `scripts/todo.sh done <id>`
  to move it into `FINISHED.toml`, dated. **Don't wait on a build to do it.** The moment the code
  and its docs are written, commit them *and* run `done <id>` in that same commit — kick the APK
  or image build off in the background and move on. If the build later fails, that's a **new**
  task (`scripts/todo.sh add …`), not a reason the finished one stayed open. Never end a turn with
  completed work sitting uncommitted and its task still active. A **partial** epic stays in `TODO.toml` (status
  `in-progress`) with its done sub-items in the description for context, and only migrates once
  every part is done. `FINISHED.toml` is history, never a worklist — nothing open belongs there.
- A stale `TODO.toml`/`FINISHED.toml` means the change isn't done — same rule as the docs. Run
  `scripts/todo.sh validate` if you ever hand-edit them.
- Per-task boolean flags (`store.FLAGS` in `.claude/skills/todo/scripts/store.py`): this project
  uses none.

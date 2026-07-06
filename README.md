# claude_spawner

A voice-driven remote control for [Claude Code](https://claude.com/claude-code).

Speak to an **Android app**, and it relays your voice to a **server** on your machine that spawns
and manages **Claude Code sessions**, driving them headless. The app is a hands-free passthrough:
say a command and it runs; attach to a session and your dictation goes straight to Claude, with
replies streamed back and read aloud.

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
- **Speech-to-text** runs on the **server** (Whisper).
- The **server** drives Claude Code **headless** (`claude -p --output-format stream-json`, with
  `--dangerously-skip-permissions`). A session is a durable `session_id` on disk, reattached each
  turn with `--resume`, so replies come back as clean structured text — never scraped from a
  terminal UI. (Design notes: [`CLAUDE.md`](./CLAUDE.md), [`docs/architecture.md`](./docs/architecture.md).)
- While **attached**, your speech is dictated into the session and Claude's reply is streamed back
  to the phone (display + text-to-speech). You can also `claude --resume <id>` in a real terminal to
  watch or take over the same session — the server detects this and warns rather than driving it
  concurrently.

## Stack

| Part        | Choice                                                              |
|-------------|---------------------------------------------------------------------|
| Server      | **Go** — WebSocket gateway, headless session manager, Whisper glue  |
| Android app | **Kotlin** — Porcupine wake word, audio capture, TTS, WS client     |
| Wake word   | **On-device** (Porcupine)                                           |
| STT         | **Server-side Whisper** (hybrid: wake on-device, dictation on server)|
| Sessions    | **headless `claude -p` (stream-json)**, durable via `session_id` on disk |
| Conflict check| **tmux** inspected to detect a `claude` a human has open in a pane      |

## Reserved commands

All prefixed with **"hey buddy"**:

- `spawn a new session` — interactive dialog for directory + attach
- `attach to <name>`
- `detach`
- `list sessions`
- `kill session <name>`
- `rename to <name>` / `call this <name>` — rename the session you're attached to
- `what's the status` / `what's it doing`
- `read last` / `read last 3` — re-read Claude's recent replies aloud
- `clear the context` — start Claude fresh **without** losing your history (see below)
- `compress the context` — like `clear`, but carries a **summary** forward (see below)

Anything spoken **while attached** that isn't a reserved command is dictated to the session. When a
command fails (a bad path, a name that's taken, a session live in a terminal…), the server speaks a
plain-language reason instead of failing silently.

**Without your voice:** swipe up on the message box for a **command tray** of tap buttons — one per
command that needs no argument (`detach`, `clear`, `compress`, `status`, `usage`, …). Open the
**sessions drawer** with the ☰ menu or by swiping in from the left edge (just inside the edge — the
very edge is Android's back gesture). The session list **auto-refreshes each time the drawer opens**,
and you can **pull down on the list to refresh** it at any time. See [`docs/commands.md`](docs/commands.md).

### Clearing vs. compressing context

Every dictated turn resumes the session with `--resume`, so Claude re-reads the whole conversation
each turn — which makes a long session progressively more expensive.

- **"hey buddy, clear the context"** rotates to a fresh `session_id`: the next turn starts Claude
  with empty context (no re-read, no re-billing). Nothing is deleted — the old transcript stays on
  disk and still scrolls back in the app; Claude just stops seeing it. Use it when starting
  unrelated work in the same directory.
- **"hey buddy, compress the context"** is the `/compact` analogue: the server has Claude summarize
  the conversation, rotates to a fresh `session_id`, and prepends that summary to your next
  dictation — so Claude keeps a compact recap instead of the full transcript. Costs one model turn.
  Use it to keep going on the same task while trimming cost.

### Token & usage displays

All screen-only (nothing spoken), so hands-free dictation is unaffected. The numbers come straight
from the headless `result` usage — no estimation. See [`docs/protocol.md`](./docs/protocol.md).

- **Token badge** under each reply (toggle in Settings → Appearance): the turn's context and output
  tokens (`24k↑ 340↓`), a **⚡** when it reused a warm prompt cache, and a detailed mode that splits
  fresh vs. cached input.
- **Cache-warm timer** — counts down the ~5-minute window in which your next turn reuses the warm
  prompt cache rather than rebuilding the whole context.
- **Title bar** shows the attached session's current context size (`🧠 24k`).
- **Session limit** at the bottom of the sessions drawer — which Claude usage window (rolling 5-hour
  or weekly) is binding and when it resets, from the CLI's `rate_limit_event` (refreshes each turn).
- **📊 Check usage** (drawer button, or "hey buddy, usage") runs `claude -p "/usage"` for the exact
  session/weekly percentages the desktop TUI's `/usage` shows; the voice form also speaks a one-line
  summary. Between checks, a free **drift estimate** (`~68%`, marked `(est)`) keeps a current-ish
  figure and snaps back to the real numbers each time you check.

Each live message also carries a small date/time badge.

## Security

The server can run arbitrary commands (Claude runs with permissions bypassed). **Do not expose it to
the internet without authentication and TLS.** Use a private network / Tailscale, require an auth
token from the app, and constrain spawn directories.

### Transport TLS and mutual TLS (optional)

By default the WebSocket is plain `ws://`, which is fine when the only hop is a Tailscale/WireGuard
tunnel (it already encrypts). To encrypt the channel independently, or to require a client
certificate on top of the shared token, set these env vars:

- **Server TLS (`wss://`)** — set `SPAWNER_TLS_CERT` and `SPAWNER_TLS_KEY` to a PEM cert/key pair
  (both or neither; one alone is a startup error). The listener then serves `wss://`; point the app
  at a `wss://…` URL. With a publicly-trusted cert (e.g. a Tailscale HTTPS/Let's Encrypt cert) the
  Android client needs no change — just the `wss://` URL.
- **Mutual TLS** — also set `SPAWNER_TLS_CLIENT_CA` to a PEM bundle of the CA(s) that sign your
  client certificates. The server then demands a valid client cert **in addition to** the token, so
  a leaked token alone can't attach. Requires the server cert/key pair. (The Android app does not yet
  ship a client certificate — mTLS is ready server-side and reachable today by CLI/`wsclient`
  clients; app-side client-cert support is tracked in `TODO.md`.)

## Where sessions run: host vs. sandbox

Each session picks an **execution target** at spawn time, a durable per-session choice:

- **host** (default) — turns run as a child process on the host, editing real host files with your
  host toolchain. No configuration needed.
- **sandbox** — turns run inside an isolated container (root *inside* the container) via a
  **rootless** runtime (Podman by default), so no host root is needed. The container is
  **persistent for the session's lifetime** — packages you install and services you start survive
  between turns — and is destroyed when you delete the session. Set `SPAWNER_SANDBOX_IMAGE` to an
  image carrying `claude` + your toolchain to enable it; the spawn dialog then adds a "host or
  sandbox?" step. The working directory is bind-mounted at the same path so edits land there. Tune
  with the other `SPAWNER_SANDBOX_*` vars. A ready-to-build Arch image and the rootless-Podman
  config live in [`sandbox/`](./sandbox/README.md).

### The live deployment: containerized server + host broker

The **server always runs in a container** while sessions execute on the host — without the container
holding host root or a runtime socket. A small **broker** daemon (`cmd/broker`) runs on the host as
your ordinary user; the server's `SPAWNER_BROKER_SOCKET` points at its Unix socket and routes **all**
turns through it. The broker is the single host-side agent for both targets: it forks `claude` for
host sessions and drives rootless Podman for sandbox sessions, enforcing the `SPAWNER_ROOT` jail — so
no component runs as root.

The live setup runs the server as a **Docker** container
([`docker-compose.broker.yml`](./docker-compose.broker.yml)) plus the broker as a **systemd user
service** ([`deploy/`](./deploy/)). To reproduce: install + enable the broker service, then
`docker compose -f docker-compose.broker.yml up -d --build`. The app's **restart** button asks the
broker to rebuild and relaunch the server container via `SPAWNER_BROKER_RESTART_CMD`. Full design in
[`docs/architecture.md`](./docs/architecture.md).

## Quick all-in-one dev container

For a local trial, `docker-compose.yml` bakes Go, the `claude` CLI, and whisper.cpp + a model into
one container that executes turns in-process (no broker) — nothing to install on the host. Source
stays bind-mounted, so you edit and version normally.

`docker compose up` starts the **spawner** plus two **resident whisper.cpp HTTP servers** (an
accurate model on `:8571`, a fast draft/detection model on `:8572`, Vulkan-built for the host AMD
GPU — see [`whisper/`](./whisper/README.md)). By default the spawner transcribes with its bundled
whisper.cpp CLI; set `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL` to prefer the resident
servers instead (as the broker deployment does).

```bash
# build (compiles whisper.cpp + fetches base.en) and run on :8080
docker compose up --build

# drive it with the text client
docker compose run --rm spawner go run ./cmd/wsclient -url ws://spawner:8080/ws
#   hey buddy spawn a new session → workspace demo → yes → then dictate to Claude Code

# real voice end-to-end with whisper's sample clip
docker compose run --rm spawner \
  go run ./cmd/wsclient -url ws://spawner:8080/ws -audio /opt/whisper.cpp/samples/jfk.wav
```

- `claude` authenticates via your host creds, mounted from `~/.claude` + `~/.claude.json` (or set
  `ANTHROPIC_API_KEY`). Sessions spawn under `/workspace`; `SPAWNER_ROOT` jails them there.
- Bigger/more-accurate model: `docker compose build --build-arg WHISPER_MODEL=small.en`.

## Project history

Built in phases: the response-capture decision and spec (Phase 0), the Go server (Phase 1),
transcription and dialog (Phase 2), the Kotlin/Compose app (Phase 3), passthrough/attach (Phase 4),
and polish (Phase 5 — auto-reconnect, barge-in, abort-a-turn, notifications, and the token/usage
displays above). All phases are complete and verified live. Active work and any remaining open items
live in the single task tracker, [`TODO.md`](./TODO.md).
</content>
</invoke>

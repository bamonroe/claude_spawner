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
  a leaked token alone can't attach. Requires the server cert/key pair. In the Android app, open
  **Settings → Server → Client certificate (mTLS)**, import your `.p12`/PKCS#12 file, and enter its
  passphrase; the app presents it on every (re)connect. A bad passphrase or corrupt file is reported
  and the app falls back to a cert-less connection.

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

### The live deployment: a bare-metal server under systemd

The **server runs bare metal** as a single Go binary under a **systemd user service**, as your
ordinary user (never root). It forks `claude` for host sessions and drives rootless Podman for
sandbox sessions itself, enforcing the `SPAWNER_ROOT` jail — so no component runs as root. There is
no separate broker: that indirection existed only to let a containerized server reach the host, and
the server never needed root, so it was folded back into this binary. The only thing still
containerized is **transcription** — two resident whisper.cpp HTTP servers ([`whisper/`](./whisper/README.md)),
an accurate model on `:8571` and a fast draft/detection model on `:8572`.

Bring-up lives in [`deploy/`](./deploy/README.md): build the binary, drop the env file, enable the
lingering user service, and start the whisper servers with `docker compose up -d whisper
whisper-fast`. The app's **restart** button fires `SPAWNER_RESTART_CMD` (set it to
[`deploy/rebuild.sh`](./deploy/rebuild.sh)), which rebuilds the binary and restarts the unit on
current code. Full design in [`docs/architecture.md`](./docs/architecture.md).

## Building & running it

Build the single binary and run it directly (no container):

```bash
# build the server (the Go module is under server/)
go build -C server -o ~/.local/bin/spawner-server .

# run it on :8080 with a spawn jail; add SPAWNER_WHISPER_URL/_FAST_URL for voice
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8080 SPAWNER_ROOT="$HOME/git:/data" \
  ~/.local/bin/spawner-server

# drive it with the text client (spawn, then dictate to Claude Code)
go run -C server ./cmd/wsclient -url ws://localhost:8080/ws
#   hey buddy spawn a new session → git demo → yes → then dictate to Claude Code
```

- `claude` authenticates via your host creds in `~/.claude` + `~/.claude.json` (or set
  `ANTHROPIC_API_KEY`). Sessions spawn under `SPAWNER_ROOT`, which jails them.
- Voice end-to-end needs the resident whisper servers running (`docker compose up -d whisper
  whisper-fast`) and `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL` pointed at them.
- To test a change without killing a live turn, run the fresh binary on a scratch port
  (`SPAWNER_ADDR=:8557`) with a separate `SPAWNER_STATE` — see [`deploy/README.md`](./deploy/README.md).

## Project history

Built in phases: the response-capture decision and spec (Phase 0), the Go server (Phase 1),
transcription and dialog (Phase 2), the Kotlin/Compose app (Phase 3), passthrough/attach (Phase 4),
and polish (Phase 5 — auto-reconnect, barge-in, abort-a-turn, notifications, and the token/usage
displays above). All phases are complete and verified live. Active work and any remaining open items
live in the single task tracker, [`TODO.md`](./TODO.md).
</content>
</invoke>

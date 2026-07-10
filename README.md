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
- `list models` / `use model <number>` — list the AI's models and switch by number (see below)

Anything spoken **while attached** that isn't a reserved command is dictated to the session. When a
command fails (a bad path, a name that's taken, a session live in a terminal…), the server speaks a
plain-language reason instead of failing silently.

**Wake and end tokens (Settings → Commands).** The two spoken tokens that bracket a command live on
the Commands settings page. The **end token** (default "beep") commits a hands-free message. The
**wake token** field lets you add your own wake word — it's accepted *alongside* the built-in "hey
buddy" (blank keeps "hey buddy" only). Pick a word Whisper transcribes cleanly: a custom word has no
curated mis-hear alias list the way "hey buddy" does, though the server does bias transcription
toward it.

**Without your voice:** swipe up on the message box — or tap the **chevron handle** just above it —
for a **command tray** of tap buttons, one per command that needs no argument (`detach`, `clear`,
`compress`, `status`, `usage`, …). Open the **sessions drawer** with the ☰ menu or by swiping in from
the left edge (just inside the edge — the very edge is Android's back gesture). The session list
**auto-refreshes each time the drawer opens**, and you can **pull down on the list** (or tap
**Refresh**) to re-scan at any time. See [`docs/commands.md`](docs/commands.md).

Each session is shown as a **card** with its name, AI backend/model, and a **sandbox** badge when
it runs in a container; the attached session's card is tinted. **Tap a card** to **expand it in
place** (tap again to collapse), revealing its **directory path** and three actions:

- **Open** — attach to the session (the same as tapping a row used to do).
- **Edit** — rename it, and (when the server advertises more than one backend) **switch its AI
  agent + model**. Changing only the model keeps the conversation; **switching the backend starts a
  fresh conversation** on the new AI (Claude and Codex transcripts aren't interchangeable on disk —
  the old history stays on disk but isn't carried over), and the dialog warns you before you commit.
- **Delete** — permanently remove the session's transcript(s) (with the same confirmation as before).

### Transferring files to and from a session

To the **left of the message box** is a transfer button (📎). Tap it to **upload** or **download** a
file over the same authenticated WebSocket — no separate share sheet or `scp`.

- **Upload:** pick a file on the phone (the system file picker), then choose a destination directory
  on the session's host — the picker opens at the **session's own directory** and browses that host's
  filesystem (the same host-scoped browser the New-session picker uses, over SSH). The file is written
  there, and the message box is **prefilled** with `look at the file at <path>` — *not sent*, so you can
  edit or add to it before dictating/hitting send.
- **Download:** the reverse — browse the host's filesystem starting at the session's directory (files
  are shown alongside folders now), pick a file, then choose where to save it on the phone.

Bytes travel base64-encoded in one message each way, capped at 64 MiB. Because the transfer runs on the
session's host over SSH, an upload lands on the very machine the session runs on (loopback for a local
session), exactly where Claude will look for it.

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

**Automatic compression** (Settings → Server) runs that compress for you. Set a token limit (in
thousands) and turn on either of two triggers that share it — the trigger is server-side, so it
fires even when the app is detached, and the preference is global (one limit for all sessions):

- **Warm compress** — once a session's context grows past the limit, fire a compress in the last
  ~15 seconds of its ~5-minute warm prompt-cache window, so the summary turn reuses the still-warm
  cache instead of paying a cold context rebuild later. Opportunistic: it waits for that edge.
- **Auto compress** — compress the moment an idle session crosses the limit, without waiting for the
  warm window. Immediate (it may pay a cold cache read); wins over warm compress if both are on.

The compress summary keeps your **most recent messages in near-verbatim detail** and squeezes older
history harder, so the active working context survives compaction.

### Choosing the AI backend and its model

The server drives more than one headless AI. Each **backend** is an entry in an AI registry that
declares how to invoke it and how to read its output, so they share one interface; two ship today:

- **Claude Code** (the default) — `claude` headless in stream-json mode.
- **Codex** (OpenAI's CLI) — `codex exec`; the server captures Codex's own session id and resumes
  it turn to turn. Needs `codex` installed and logged in (`codex login`); set `SPAWNER_CODEX_BIN` if
  it isn't on the server's `PATH` (and `SPAWNER_SANDBOX_CODEX_BIN` / `SPAWNER_SSH_CODEX_BIN` for the
  sandbox and SSH targets, analogous to the per-target Claude binaries).

Pick the backend when you spawn — by **voice**, "hey buddy, spawn a codex session" (or "…on codex")
creates a Codex session; a plain spawn uses Claude. In the **visual New-session picker** (the app or
the browser client), a backend chip row (shown when more than one backend is available) and a model
chip row let you choose both before starting. The new session is stamped with that backend and its
default model.

A session records which backend it runs and which **model**. Each backend has a **default model**
the spawner picks for you, plus a short catalogue you can switch between by voice:

- **"hey buddy, list models"** — speaks the attached session's backend catalogue, numbered, marking
  the current one (Claude: `opus` / `sonnet` / `fable`; Codex on a ChatGPT-account plan: `gpt-5.5`
  and its low/high reasoning presets — the account decides which model ids are selectable).
- **"hey buddy, use model 2"** — switches to that numbered model (say the number — "two" or "2").
  Selecting by **number** is deliberate: it sidesteps having to pronounce awkward model names. The
  choice is durable on the session and takes effect on your next message.

Each session's backend and model are also shown on screen: the sessions drawer tags every row with a
small **"Backend · model"** badge (the backend name is dropped for the default Claude, so a
single-backend setup just shows the model), and the title bar shows the attached session's badge next
to the context meter.

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

**In the common deployment, TLS is terminated at a reverse proxy (Caddy) in front of the server:**
the proxy serves `wss://` with a publicly-trusted cert and forwards plain `ws://` to the spawner on
localhost. The app just points at the proxy's `wss://…` URL and authenticates with the token — there
is **no client certificate to install in the app** (removed; if you need mutual TLS, enforce it at
the proxy). By default, with no proxy, the WebSocket is plain `ws://`, which is fine when the only
hop is a Tailscale/WireGuard tunnel (it already encrypts).

The server can also do TLS itself (for setups without a proxy) via these env vars:

- **Server TLS (`wss://`)** — set `SPAWNER_TLS_CERT` and `SPAWNER_TLS_KEY` to a PEM cert/key pair
  (both or neither; one alone is a startup error). The listener then serves `wss://`; point the app
  at a `wss://…` URL.
- **Mutual TLS** — also set `SPAWNER_TLS_CLIENT_CA` to a PEM bundle of the CA(s) that sign your
  client certificates. The server then demands a valid client cert **in addition to** the token, so
  a leaked token alone can't attach (requires the server cert/key pair). The app itself no longer
  presents a client cert, so this path is for non-app clients or is better handled at the proxy.

## Where sessions run: host vs. sandbox

Each session picks an **execution target** at spawn time, a durable per-session choice:

- **host** (default) — turns run as a child process on the host, editing real host files with your
  host toolchain. No configuration needed.
- **sandbox** — turns run inside an isolated container (root *inside* the container) via a
  **rootless** runtime (Podman by default), so no host root is needed. The container is
  **persistent for the session's lifetime** — packages you install and services you start survive
  between turns — and is destroyed when you delete the session. Set `SPAWNER_SANDBOX_IMAGE` to an
  image carrying `claude` + your toolchain to enable it; the voice spawn dialog then adds a "host or
  sandbox?" step, and the visual sidebar's new-session screen shows a **host/sandbox toggle** (host
  by default) so you can pick the target when starting a project there too. The working directory is bind-mounted at the same path so edits land there, and
  the server's whole `$HOME` is bind-mounted **read-write at the same path** by default so your
  dotfiles, `~/.claude`, and checkouts are available and writable in the container just like on the
  host. Tune with the other `SPAWNER_SANDBOX_*` vars. A ready-to-build Arch image and the rootless-Podman
  config live in [`sandbox/`](./sandbox/README.md). On a **containerized, SSH-native server**
  (`SPAWNER_SSH=1`) the sandbox works too: the container has no runtime of its own, so it drives
  rootless Podman **on the host over SSH** (the same connection host turns use) — set the
  `SPAWNER_SANDBOX_*` vars in the container env as host paths, keep `HOME` pointed at the host user's
  home, and sandbox sessions run there just as they do bare-metal.

### The live deployment: a bare-metal server under systemd

The **server runs bare metal** as a single Go binary under a **systemd user service**, as your
ordinary user (never root). It forks `claude` for host sessions and drives rootless Podman for
sandbox sessions itself, enforcing the `SPAWNER_ROOT` jail — so no component runs as root. There is
no separate broker: that indirection existed only to let a containerized server reach the host, and
the server never needed root, so it was folded back into this binary. The only thing still
containerized is **transcription** — two resident whisper.cpp HTTP servers ([`whisper/`](./whisper/README.md)),
an accurate model on `:8571` and a fast draft/detection model on `:8572`. The accurate model is
server-global and can be hot-swapped from **Settings → Server** (it loads for every device at once);
that choice is **persisted to `settings.json`** next to the session state, so a restart or rebuild
keeps it instead of reverting to `SPAWNER_WHISPER_MODEL_NAME`. (Settings the app owns — the
per-device voice prefs — ride along in each `hello` and don't need server-side storage.)

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

### The browser client (Compose Multiplatform)

The same UI as the Android app also runs **in a browser** via Kotlin/Wasm — one shared `commonMain`
renders identical composables on both. Build the web bundle and let the server host it:

```bash
# build the web bundle (index.html + spawnerweb.js + .wasm) — needs JDK 21
./android/gradlew -p android :app:wasmJsBrowserDistribution
#   output: android/app/build/dist/wasmJs/productionExecutable/

# point the server at it — served at "/" alongside the "/ws" gateway (one binary)
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8080 SPAWNER_ROOT="$HOME/git:/data" \
  SPAWNER_WEB_DIR=android/app/build/dist/wasmJs/productionExecutable \
  ~/.local/bin/spawner-server
#   then open http://<host>:8080/ in a browser (needs a Wasm-GC browser — recent Firefox/Chrome)
```

The bundle defaults its WebSocket to the **same origin** it was served from (`/ws`, `wss://` when the
page is https), so a server-hosted client connects with no setup — you only edit the URL/token under
**Settings → Server** if you're pointing elsewhere. The static assets are public; the privileged
surface stays behind the token-authenticated `/ws` handshake (and mutual TLS if configured).

Text chat, the session drawer, hosts/identities, usage, **file transfer** (the 📎 button — the same
upload/download flow as the app, reading/writing the browser's own files), and **spawning new
sessions** (the same New-session picker as the app — target/host + backend/model + filesystem browse,
sharing one `commonMain` `BrowseScreen`) all work. Because a mouse can't obviously "swipe", the
browser client also shows **visible controls** for the touch gestures: a chevron handle above the
message box opens the command tray, a **Refresh** button sits beside **New** in the sessions drawer,
and **Enter sends** a message (Shift+Enter for a newline). Browser audio (mic/STT and spoken replies)
is not wired yet.

> **Secure context required.** The client only connects from a **secure context** — https, or
> `localhost`/`127.0.0.1`. Served over plain http from a real hostname the browser marks the origin
> insecure and the connection fails, so put the server behind TLS (a `wss://` cert, or a reverse proxy
> like Caddy) for anything but local testing.

## Project history

Built in phases: the response-capture decision and spec (Phase 0), the Go server (Phase 1),
transcription and dialog (Phase 2), the Kotlin/Compose app (Phase 3), passthrough/attach (Phase 4),
and polish (Phase 5 — auto-reconnect, barge-in, abort-a-turn, notifications, and the token/usage
displays above). All phases are complete and verified live. Active work and any remaining open items
live in the single task tracker, [`TODO.md`](./TODO.md).
</content>
</invoke>

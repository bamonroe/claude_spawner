# claude_spawner

A voice-driven remote control for [Claude Code](https://claude.com/claude-code).

Speak to an **Android app**, and it relays your voice to a **server** on your machine that
spawns and manages **tmux sessions** running Claude Code. The app is a hands-free passthrough:
say a command and it's executed; attach to a session and your dictation goes straight to Claude,
with Claude's replies streamed back and read aloud.

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
- **Speech-to-text** runs on the **server** (Whisper) for accuracy.
- The **server** drives Claude Code **headless** (`claude -p --output-format stream-json`) with
  `--dangerously-skip-permissions`. A session is a durable `session_id` on disk, reattached each
  turn with `--resume` — so responses come back as clean structured text, never scraped from a
  terminal UI.
- While **attached**, your speech is dictated into the session and Claude's response is
  streamed back to the phone (display + text-to-speech).
- Optionally, you can run `claude --resume <id>` in a real terminal yourself to watch or take over
  the **same** on-disk session (the server detects this and warns rather than driving it at the
  same time).

## Stack

| Part        | Choice                                                              |
|-------------|---------------------------------------------------------------------|
| Server      | **Go** — WebSocket gateway, tmux session manager, Whisper glue      |
| Android app | **Kotlin** — Porcupine wake word, audio capture, TTS, WS client     |
| Wake word   | **On-device** (Porcupine)                                           |
| STT         | **Server-side Whisper** (hybrid: wake on-device, dictation on server)|
| Sessions    | **headless `claude -p` (stream-json)**, durable via `session_id` on disk |
| Conflict check| **tmux** inspected to detect a `claude` a human has open in a pane      |

See [`CLAUDE.md`](./CLAUDE.md) for the full architecture, data flow, and design notes.

## Reserved commands (planned)

All prefixed with **"hey buddy"**:

- `spawn a new session` — interactive dialog for directory + attach
- `attach to <name>`
- `detach`
- `list sessions`
- `kill session <name>`
- `what's the status` / `what's it doing`
- `read last` / `read last 3` — re-read Claude's recent replies aloud
- `clear the context` — start Claude fresh **without** losing your history (see below)
- `compress the context` — like `clear`, but carries a **summary** forward instead of dropping context (see below)

Anything spoken **while attached** that isn't a reserved command is dictated to the session.

### Clearing context (keep history, stop paying to replay it)

A session is a durable `session_id` on disk, and every dictated turn resumes it with `--resume` —
which means Claude re-reads the **entire** conversation each turn (that's how it keeps context, and
it's what makes a long session progressively more expensive per turn).

Saying **"hey buddy, clear the context"** (or "clear context" / "start fresh") **rotates** the
session instead of deleting it: the current `session_id` is retired and a fresh one takes over, so
the next thing you say starts Claude with an empty context — no re-read, no re-billing of the whole
transcript. The retired transcript is **kept on disk**, so the app still shows your full history;
the server just stitches the retired and current transcripts together when you scroll back. Claude
simply stops seeing the old turns.

Use it whenever you've finished one line of work and want to start another in the same directory
without carrying (and paying for) all the prior context. It never deletes anything — "clear
history" is intentionally *not* a command, because clearing keeps the history.

### Compressing context (keep going, but condensed)

Sometimes you *don't* want to drop the context — you're mid-task and Claude still needs to know what
you've been doing — you just want to stop replaying the whole long transcript every turn. Saying
**"hey buddy, compress the context"** (or "compact" / "condense context") is the `/compact` analogue
of `clear`: the server asks Claude to **summarize** the conversation so far, then rotates to a fresh
`session_id` exactly like `clear` — but stashes that summary and **prepends it to your next
dictation**. So Claude picks up with a compact recap of the task, decisions, and current state
instead of either the full (expensive) transcript or a blank slate.

It costs **one** model turn (the summary) and, like `clear`, keeps the old transcript on disk so your
full history still scrolls back. Reach for `clear` when you're starting something unrelated, and
`compress` when you want to keep going on the same work but trim the running cost.

### Seeing token usage (and the warm-cache window)

Each turn's token cost rides back on the reply, so the app can show what a turn actually used.
Two independent, toggleable displays live in **Settings → Appearance**:

- **Token badge** — a small caption under each Claude reply. **Compact** (the default) shows the
  turn's total context tokens and Claude's output (`24k↑ 340↓`), plus a **⚡** when the turn reused a
  warm prompt cache. **Detailed** breaks the input apart — fresh input vs. `cached` (a cheap
  cache-read) vs. newly-cached (`new`) tokens — then the output. **Off** hides it entirely.
- **Cache-warm timer** — a status-bar line counting down the ~5-minute window during which your
  **next** turn will reuse the warm prompt cache (`⚡ cache warm · 3:12 left`) rather than paying to
  rebuild the whole context (`❄ cache cold — next turn rebuilds context`). Each turn resets the
  countdown; attaching to a different session resets it (that session has its own, cold, cache).

Nothing here is spoken — it's screen-only, so hands-free dictation is unaffected. The numbers come
straight from the headless `result` event's usage (no estimation); see the `output.usage` field in
[`docs/protocol.md`](./docs/protocol.md).

### Seeing your Claude plan's session limit

At the **bottom of the sessions drawer** (the ☰ menu) is a readout of your Claude subscription's
usage window — e.g. `⏳ Claude 5-hour session limit · resets 3:00pm · in 2h 13m`. It comes from the
`rate_limit_event` the headless CLI emits early in every turn, so it refreshes each turn (no polling).
`limit_type` tells you which window is binding — the rolling **5-hour session** window or the **weekly**
cap — and `resets_at` is the exact reset time. If the status leaves `allowed` (you're nearing/at the
cap) the line turns amber; an overage note appears if you're drawing on pay-as-you-go credits.

One honest caveat: Anthropic exposes only a **coarse** status here, not an exact remaining quota — so
this shows *which* limit and *when it resets*, reliably, but not a "62% used" fuel gauge. Details in
the `rate_limit` message in [`docs/protocol.md`](./docs/protocol.md).

## How responses are captured (the once-hard problem, now solved)

Claude Code's interactive TUI would be miserable to screen-scrape (ANSI, redraws, spinners), so
we **don't**. The server drives Claude **headless** in `stream-json` mode and reads the clean
`result` event for each turn — verified end-to-end against `claude` 2.1.196, including multi-turn
memory via `--resume`. Details in [`CLAUDE.md`](./CLAUDE.md#-resolved-how-we-capture-claudes-responses-do-not-scrape-the-tui).

## Security note

The server can run arbitrary commands (Claude runs with permissions bypassed). **Do not expose
it to the internet without authentication and TLS.** Use a private network / Tailscale, require
an auth token from the app, and constrain spawn directories.

---

## Run it in Docker (recommended)

The whole execution environment — Go, `tmux`, the `claude` CLI, and **whisper.cpp + a model** —
is baked into a container so nothing has to be installed on the host. Source stays on the host
(bind-mounted), so you edit and version normally; only execution is containerized.

`docker compose up` starts three services: the **spawner** and two **resident whisper.cpp HTTP
servers** — an accurate model on `:8571` and a fast draft/detection model on `:8572`, built with
Vulkan for the host's AMD GPU (see [`whisper/`](./whisper/README.md)). In this compose setup the
spawner transcribes with its **bundled whisper.cpp CLI** (a model is baked into its image); it does
*not* auto-wire to the resident servers. To use them instead, set `SPAWNER_WHISPER_URL` /
`SPAWNER_WHISPER_FAST_URL` on the spawner — which is exactly what the host-native/systemd
deployment does (see [`deploy/spawner.env.example`](./deploy/spawner.env.example)). When those URLs
are set the server prefers the resident servers and falls back to the CLI. Engine details are in
[`CLAUDE.md`](./CLAUDE.md).

```bash
# build (compiles whisper.cpp + fetches a model; base.en by default) and run on :8080
docker compose up --build

# drive it with the text client (from inside the same environment)
docker compose run --rm spawner go run ./cmd/wsclient -url ws://spawner:8080/ws
#   hey buddy spawn a new session
#   workspace demo
#   yes
#   <anything> -> dictated to Claude Code; reply comes back as 💬

# test real voice transcription end-to-end with whisper's sample clip
docker compose run --rm spawner \
  go run ./cmd/wsclient -url ws://spawner:8080/ws -audio /opt/whisper.cpp/samples/jfk.wav
```

- `claude` inside the container authenticates via your host creds, mounted from `~/.claude` +
  `~/.claude.json` (or set `ANTHROPIC_API_KEY` in your shell). See `docker-compose.yml`.
- Sessions are spawned under `/workspace` (a persisted volume); `SPAWNER_ROOT` jails them there.
- Bigger/more-accurate model: `docker compose build --build-arg WHISPER_MODEL=small.en`.

## Try it on the host (no Docker)

```bash
cd server
mkdir -p /tmp/sandbox
SPAWNER_TOKEN=secret SPAWNER_ROOT=/tmp/sandbox go run .          # text path only
# add SPAWNER_WHISPER_MODEL=/path/to/ggml-small.en.bin to enable voice
```

Then in another terminal, `SPAWNER_TOKEN=secret go run ./cmd/wsclient` and type utterances.
Requires the `claude` CLI and `tmux` on the host. Transcription is **off** unless a whisper model
or URL is configured (text utterances work either way); the full config-var list lives in
[`CLAUDE.md`](./CLAUDE.md). Audio in is PCM16LE / 16 kHz / mono (see `docs/protocol.md`).

To run it as a long-lived service (systemd unit + the resident GPU whisper servers) instead of
`go run`, see [`deploy/`](./deploy/README.md).

---

## To-Do / Roadmap

> This is the **historical, phase-by-phase record** of how the project was built. For the live
> list of what's in flight or recently finished, see [`TODO.md`](./TODO.md) — that's the
> authoritative task tracker; this section is kept as a completed-phases narrative.

### Phase 0 — Decisions & spec ✅
- [x] Response-capture decision: **headless `claude -p --output-format stream-json`**, durable
      `session_id` on disk + `--resume` — verified end-to-end. (No TUI scraping.)
- [x] `docs/protocol.md` — WebSocket message schema
- [x] `docs/commands.md` — "hey buddy" command grammar + dialog flows
- [→] Auth mechanism / transport beyond the shared token (mTLS? Tailscale? reverse proxy?) —
      still open; tracked in [`TODO.md`](./TODO.md).

### Phase 1 — Server skeleton (Go)
- [x] Project scaffold (`/server`), env config, graceful shutdown, `/healthz`
- [x] `internal/session`: headless `claude` driver (`Driver.Turn`, stream-json parsing,
      tool-event breadcrumbs, error subtypes) + `NewSessionID`
- [x] `internal/session`: durable, file-backed session registry (`Store`) with atomic writes
- [x] `internal/tmux`: detect a live interactive `claude` in a pane (`ClaudeDirs`) for conflict warnings
- [x] Spawn-path validation against an allowed root (`config.ValidateSpawnDir`, tested)
- [x] WebSocket gateway + auth handshake (`internal/gateway`, gorilla/websocket)
- [x] `spawn` action: generate `session_id`, mkdir (validated), persist record
- [x] Wire `list` / `attach` / `detach` / `kill` actions to the store + driver
- [x] Dictation turn loop: attached utterance → `Driver.Turn` → `output` (async, tool breadcrumbs)
- [x] `cmd/wsclient`: interactive text client for manual testing (no app needed)
- [x] **Verified live**: spawn → attach → dictate → real claude reply → `--resume` recall

### Phase 2 — Transcription & dialog
- [x] Command parser: control command vs passthrough dictation (`internal/command`)
- [x] Dialog state machine (the "ok bud, where?" → "want to attach?" flow, `internal/gateway`)
- [x] Spoken-path normalization (fuzzy dir matching in `internal/projects`; STT is lowercase — see CLAUDE.md)
- [x] Audio-stream ingest: `wake` + binary PCM16 frames + `audio_end` → WAV → transcript → `utterance`
- [x] Whisper integration behind a `Transcriber` interface (`internal/transcribe`, whisper.cpp shell-out)
- [x] Dockerized dev environment (Go + tmux + claude CLI + whisper.cpp + model)
- [x] Verify a real audio turn end-to-end (jfk.wav / spoken clip → transcript → Claude reply),
      on both the resident GPU whisper server and the CLI fallback
- [x] Vocab biasing (`--prompt`) to improve recognition of session names / paths

### Phase 3 — Android app (Kotlin / Compose)
- [x] Project scaffold (`/android`), mic permission, foreground-service stub
- [x] Compose UI: connect, push-to-talk, text-utterance input, conversation log
- [x] WebSocket client (OkHttp) speaking the protocol; hello/auth handshake
- [x] Audio capture (PCM16/16k/mono) streamed over the voice path (wake→frames→audio_end)
- [x] Receive transcripts / dialog / session output; TTS playback of say/output
- [x] **Verified live on emulator + phone**: app → server → real Claude reply, full spawn/attach/dictate
- [x] Always-listening **hands-free** mode (server-side wake-word detection in the transcript;
      Porcupine on-device was dropped) via a mic `VoiceService`
- [→] Verify the hands-free voice model on a real device (built, not yet voice-tested) —
      tracked in [`TODO.md`](./TODO.md).

### Phase 4 — Passthrough & attach ✅
- [x] Attach binds voice I/O to a session; dictation becomes the prompt for `Driver.Turn`
- [x] Stream the `result` text + tool breadcrumbs back as `output`/`activity` messages
- [x] Detach / switch sessions; **live fan-out to all devices** attached to a session
- [x] Read Claude's responses aloud; **audio-output picker** (earpiece/speaker/Bluetooth)

### Phase 5 — Polish
- [x] Auto-connect + auto-reconnect with backoff; resume last session / in-progress dialog
- [x] Barge-in ("hey bud stop" / push-to-talk halts TTS); markdown stripped from speech
- [x] Robust turns across disconnect + **server keepalive**; **abort a running turn**;
      turns interrupted by a restart are flagged
- [x] **Busy flags** + quick voice switching ("attach to X"); **post-turn diff summary**
- [x] Persist session list across server restarts (durable `session_id`s in the store)
- [x] Whisper **vocab biasing** toward session names; **brief-reply** toggle for TTS
- [x] **Notifications** when a backgrounded turn finishes

### What's left

Open work (spoken error feedback, per-session voice naming, TLS/mTLS, on-device fallback STT, iOS)
is **not listed here** — it lives in [`TODO.md`](./TODO.md), the single live task tracker, so it
doesn't drift against this historical record.

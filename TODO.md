# TODO — claude_spawner

The **live task list** for active and recently-completed work. This is the single source of
truth for what's in flight; `README.md` keeps the historical phase-by-phase roadmap.

**Maintenance rule** (see `CLAUDE.md`): edit this file in the same commit that proposes or
completes a feature **or a test**. Adding a feature/test → add an unchecked box here. Finishing
one → check it off (move to _Done_, dated). Dropping a test/feature → remove it with a one-line
why. A change that leaves this file stale is incomplete.

Dates are `YYYY-MM-DD`.

## Active

### Server / infra
- [ ] Decide + implement the auth/transport story beyond the shared token: **TLS/mTLS** (today a
      constant-time-compared shared token, fronted by Tailscale).
- [ ] Vocab-bias tuning: measure whether the `--prompt` session-name biasing actually improves
      recognition of real session names/paths, adjust if not. *(biasing itself is implemented)*
- [ ] More spoken error feedback — surface `docs/protocol.md` error codes as friendly speech
      ("that directory doesn't exist, bud") instead of generic/silent failures.

### Android
- [ ] Verify the hands-free voice model on a real device end-to-end (built; not yet voice-tested
      on the Pixel 8a for the always-listening path specifically).
- [ ] Per-session **naming by voice** (rename exists in the app UI + as `rename`/`rename_discovered`
      messages, but there's no "hey buddy" voice command for it yet).

### Later / nice-to-have
- [ ] On-device fallback STT when offline.
- [ ] iOS app.

## Done

- [x] 2026-07-03 — Android: hands-free toggle moved onto the mic button. Removed the top-bar 🎧
      switch; **hold the mic button and swipe up** to toggle hands-free (a swipe-up during a
      push-to-talk hold abandons that clip and flips hands-free instead). Custom `awaitEachGesture`
      in `InputBar`, plus `VoiceController.cancelTalking()` to discard the aborted PTT clip.
- [x] 2026-07-03 — Deploy docs: added `deploy/README.md` (systemd unit, env-file install steps,
      `claude-log.sh` usage); linked from the root README's host section and the CLAUDE.md layout.
      This completes documentation of the root-level tree.
- [x] 2026-07-03 — Whisper docs: added `whisper/README.md` documenting the two resident-server
      images (Vulkan/GPU vs CPU), their `/inference` + `/load` API, port/model-mount contract, and
      the two deployment modes. Fixed a README inaccuracy that implied the Dockerized spawner uses
      the resident servers — under `docker compose up` it uses the bundled CLI; the resident
      servers are wired in by the host-native/systemd deploy (`deploy/spawner.env.example`).
- [x] 2026-07-03 — `android/README.md` audited against the Kotlin source and corrected: fixed the
      PCM16-vs-Opus codec contradiction (voice is captured as PCM16, encoded to Ogg/Opus on
      device, sent as Opus); added 6 omitted source files + the `generateCommands` task to the
      layout; documented the build-time-generated Commands screen; added the missing INTERNET /
      FOREGROUND_SERVICE / FOREGROUND_SERVICE_MICROPHONE permissions.
- [x] 2026-07-03 — Anti-drift consolidation: one authoritative home per fact (documentation map
      at the top of `CLAUDE.md`); status/tasks de-duplicated to `TODO.md` only (README roadmap is
      now history-only, CLAUDE.md status is a pointer); new `internal/docsync` test package fails
      `go test ./...` when env vars / wire messages / error codes drift from `docs/protocol.md` +
      `CLAUDE.md`.
- [x] 2026-07-03 — Documentation reconciliation pass: `TODO.md` introduced; `CLAUDE.md`,
      `README.md`, `docs/protocol.md`, `docs/commands.md` brought back in sync with the code
      (resident GPU whisper server, full env-var list, all wire messages + error codes, `help`
      command, real-audio-turn verified).
- [x] Command registry as the single source of truth (`command.Registry` → `docs/commands.json`
      → Android `generateCommands` build task); drift-tested.
- [x] `clear` command: rotate a session's Claude context to a fresh `session_id`, keeping the old
      transcript for `history`.
- [x] Resident GPU (Vulkan/RX 550) whisper HTTP server + fast draft model, behind the
      `Transcriber` interface, with the whisper.cpp CLI as fallback.
- [x] Real audio turn verified end-to-end (jfk.wav / spoken clip → transcript → Claude reply).
- [x] Live output streaming (per-assistant-message `output` chunks) + Android live rendering.
- [x] Hands-free always-listening mode: server-side wake-word + end-token in the transcript,
      live `pending` draft, end-token commit (Porcupine on-device was dropped).
- [x] Interactive mode: Claude's clarifying questions delivered as a structured `ask`.
- [x] Post-turn `diff` summary; running-turn `activity`/`files` breadcrumbs.
- [x] Abort a running turn; restart-interrupted turns flagged (`turn_stopped`/`turn_interrupted`).
- [x] Discover / adopt / rename / delete sessions found on disk (`~/.claude/projects`).
- [x] Durable file-backed session store; auto-connect/reconnect with backoff.
- [x] Whisper vocab biasing toward session names; brief-reply TTS toggle; finished-turn
      notifications; audio-output picker (earpiece/speaker/Bluetooth); barge-in.
- [x] Whole server-side voice pipeline + Android app verified live (emulator + Pixel 8a).

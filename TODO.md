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
- [ ] Plumb the wake-token alias list (`command.wakePhrases`) through the same pipeline as command
      aliases (→ `docs/commands.json` → `generateCommands` → app), so wake mishearings are visible
      and **editable in the app's alias editor** like regular commands. Server list is authoritative
      today; this makes it user-tunable on-device.
- [ ] On-device fallback STT when offline.
- [ ] iOS app.

## Done

- [x] 2026-07-04 — **Claude plan session-limit readout** at the bottom of the sessions drawer. Server
      parses the stream-json `rate_limit_event` (status / resetsAt / rateLimitType / isUsingOverage)
      via a new `onRateLimit` callback on `Driver.Turn` and broadcasts it as a `rate_limit` message
      (docs/protocol.md + docsync). The app shows which usage window is binding (`five_hour` / weekly)
      and when it resets, amber when status leaves `allowed`. Status is coarse (no exact quota exists).
      Server emit verified live via a scratch instance; Android wiring verified on the emulator (badge +
      cache-warm timer confirmed live too). README + docs. **Live :8555 deploy pending a spawner
      service restart** (not done in-session — this session runs through that server).
- [x] 2026-07-04 — **Per-turn token usage** surfaced to the app. Server parses the stream-json
      `result` event's aggregate `usage` (input/output/cache-write/cache-read) and carries it on the
      final `output` message (`output.usage`, docs/protocol.md + docsync). Android renders it two
      ways, both toggleable in **Settings → Appearance**: a per-reply **token badge** (Off / Compact /
      Detailed; compact is default) and a status-bar **cache-warm timer** counting down the ~5-min
      warm prompt-cache window. Screen-only (not spoken). README + emulator-verified UI.
- [x] 2026-07-04 — Wake token as a data-driven **alias list** (`command.wakePhrases`, single source
      of truth, the wake-word analogue of a command's aliases). Generalized the matcher to
      variable-width phrases so **one-word collapses** whisper produces for "hey buddy" — notably
      **"everybody"** — now fire the wake, not just two-word "hey X" mishearings. Fixes "everybody
      detach" (a real live mishearing) falling through to dictation. Tests + `docs/commands.md`.
- [x] 2026-07-04 — Android send-button UX: visible **drag track** above the mic showing how far to
      drag up for hands-free; **tap** the headset to turn hands-free off (was: swipe up again);
      **red headset** while hands-free is live.
- [x] 2026-07-03 — `compress` command: the `/compact` analogue of `clear`. Runs a background turn
      asking Claude to summarize the conversation, rotates to a fresh `session_id` (old transcript
      kept for `history`, like clear), and stashes the summary as a new durable `Session.PendingSeed`
      that `dictate` prepends to the next turn — so context is carried forward condensed instead of
      dropped. New `startCompress` job (abortable, single-writer), `compress` wire message + voice
      command, `compress_failed` error code; docs + drift-tested command registry updated.
- [x] 2026-07-03 — Interactive mode: send the ask instruction only on the first turn of a context,
      not every turn. Claude keeps it via `--resume`, so re-appending it each turn just burned
      tokens. New durable `Session.AskPrimed` flag (set on the first interactive turn's success,
      reset by `clear`); `startTurn` takes a `primeAsk` bool.
- [x] 2026-07-03 — Chat: keep the newest message fully visible when a below-list status bar
      (speaking / activity / draft / mic) appears. Those bars are Column siblings, so showing one
      shrank the list and hid the tail of the last message; ChatList now re-pins to the newest
      message when the bar set toggles (only if already at the bottom).
- [x] 2026-07-03 — Server restart from the app. New `restart` wire message: the server broadcasts a
      spoken notice, then exits non-zero so its systemd supervisor (ExecStartPre rebuilds) relaunches
      it on current code; the app auto-reconnects. Added a **Restart Server** button (confirm dialog,
      connected-only) to Settings → Server. Verified on the emulator.
- [x] 2026-07-03 — Fix orphaned hands-free draft. `stopHandsFree()` never cleared `_pending`, so
      toggling hands-free off mid-draft (easy now via the mic-button swipe-up) left the greyed draft
      line stuck above the input box — and the server kept its buffered audio, which would bleed
      into the next capture. Added a `discard_draft` wire message: the client clears the draft +
      tells the server to drop its buffer on stop.
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

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

- [x] 2026-07-04 — **Command tray: fire argument-free "hey buddy" commands by hand.** Swipe up on
      the message box to reveal a tray of tap buttons above it, one per no-arg command (`abort`,
      `cancel`, `clear`, `compress`, `detach`, `help`, `list`, `read last`, `status`, `stop`,
      `usage`); a tap sends the command (wake-prefixed, so the server parses it as a control command
      even while attached) and closes the tray; swipe down, a tap anywhere outside the tray, or
      focusing the message box to type all dismiss it. Buttons are derived from the
      generated `COMMANDS` list, excluding any command whose aliases take a `<placeholder>`
      (`attach`/`kill`/`spawn`), so the tray never drifts from the grammar. `InputBar` +
      `CommandTray` in `MainActivity.kt`. Verified live on the emulator (attached to a real session:
      the `status` button returned the attach status, not dictation).

- [x] 2026-07-04 — **Usage estimate: discount cache reads in the per-turn token cost.** `tokenCost`
      (gateway/jobs.go) was summing `cache_read` at full weight, but a warm turn re-reads the whole
      cached context (~1M tokens on a big session) that Anthropic meters at ~0.1×. So one turn drifted
      the estimate ~10× too fast and pegged it at 100% a turn or two after a `/usage` snap. Weight
      `cache_read`×0.10 and `cache_write`×1.25 to track real plan consumption; the existing 40k
      tokens/% seed already assumed a discounted measure, so this makes tokenCost and the seed
      consistent. New `TestTokenCostDiscountsCacheRead`. (The persisted `sess_rate`/`week_rate` learned
      under the old weighting are ~10× high and self-heal via `/usage` EMA, or reset cleanly on the next
      spawner restart.)
- [x] 2026-07-04 — **Chat: keep the newest message pinned above the keyboard AND the status bars**
      (supersedes/unifies the two earlier same-day re-pin fixes — the `barsKey` toggle and the
      `WindowInsets.ime` follow — which each handled only one shrink source and, for the keyboard,
      sampled `atBottom` *after* the shrink had already pushed the tail out of view). Root cause: the
      soft keyboard (via the outer Column's `imePadding()` under `adjustResize`) and the below-list
      status bars (speaking / activity / draft / mic / warm) all shrink the weighted `ChatList` from
      the bottom, and a `LazyColumn` does not follow its own shrinking viewport. `ChatList` now watches
      its **viewport height** via `snapshotFlow` and, on any change, snaps to the newest message — but
      only if it was parked at the bottom *before* the resize (`pinned` is updated only while the
      viewport is stable, so a big keyboard shrink cannot flip it first). The re-pin uses
      `scrollToItem(bottom, Int.MAX_VALUE)` — a large offset that clamps to max scroll — so the tail of
      a message TALLER than the keyboard-shrunk viewport sits just above the keyboard (a plain
      `scrollToItem(bottom)` top-aligns it and hides the bottom half; that was the "covers the bottom
      half of the message on fresh launch" bug). Scrolled up reading history → stays put. Subsumes the
      earlier stale-`bottom` clip regression too (no more `barsKey`). Verified live on the emulator
      against the real server: at-bottom rides up, scrolled-up stays put.

- [x] 2026-07-04 — **Drift-live usage estimate** across all sessions/clients. New
      `internal/usage.Estimator` (server-global, persisted next to sessions.json): every turn adds its
      weighted token cost to a running odometer and nudges the estimated session/weekly % up via a
      tokens-per-percent rate **learned from successive /usage calibrations** (first real observation
      replaces the seed, later ones EMA-blend); running /usage snaps the estimate to the real numbers.
      A forward jump in the 5-hour reset time restarts the session drift from zero. Broadcast to all
      clients (new `usage_estimate` message) after each turn, on /usage, and on connect. Shown as a
      `📊 Session ~68% · Week ~43% (est)` line at the bottom of the drawer + a "Live estimate" section
      in the usage sheet. Estimator unit-tested; verified live drift→snap→drift on the emulator.
- [x] 2026-07-04 — **`usage` command** — see exactly how much of the Claude plan is left (the TUI
      `/usage` numbers). Voice ("usage" / "how much usage left") or the 📊 Check usage button in the
      drawer; the server runs `claude -p "/usage"` (new `Driver.Usage`), parses session/weekly % used
      + resets, and returns a `usage` report. The app shows a sheet with percent-used bars + the full
      contributing breakdown; the voice form also speaks a summary. On-demand (a real claude call),
      unlike the free per-turn drawer readout. Verified end-to-end on the emulator against the new
      binary. Also fixed the drawer footer being clipped by the nav bar (navigationBarsPadding).
      Command registry + commands.json + docs/protocol.md + README.
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

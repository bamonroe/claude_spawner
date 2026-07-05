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

### Android
- (nothing open — hands-free verified; voice rename shipped, see _Done_)

### Later / nice-to-have
- [ ] Plumb the wake-token alias list (`command.wakePhrases`) through the same pipeline as command
      aliases (→ `docs/commands.json` → `generateCommands` → app), so wake mishearings are visible
      and **editable in the app's alias editor** like regular commands. Server list is authoritative
      today; this makes it user-tunable on-device.
- [ ] On-device fallback STT when offline.
- [ ] iOS app.

## Done

- [x] 2026-07-05 — **Sidebar rename now updates the attached-session title bar.** Renaming the
      attached session (sidebar `rename_discovered` or the `rename` voice command) refreshed the
      drawer list but left the title bar on the old name — the title reads `attachedName`, set only
      by the `attached` message, which a rename never re-sent. `doRename` now emits a lightweight
      `renamed` (`{old, name}`) message when the rename follows this connection's attached session;
      the app updates the title in place (and migrates the name-keyed log buffer) with no history
      refetch / meter reseed. New wire message documented in `docs/protocol.md`.
- [x] 2026-07-04 — **Open the sessions drawer with a left-edge swipe.** Besides the ☰ button, a
      narrow strip pinned to the far-left edge opens the navigation drawer on a rightward drag
      (`detectHorizontalDragGestures` overlay in `MainActivity.kt`). The drawer's built-in gestures
      stay limited to swipe-to-close (`gesturesEnabled = drawerState.isOpen`) so a horizontal drag
      across the chat can't open it, and the strip sits opposite the mic button (bottom-right) so it
      doesn't steal touches. Start just inside the edge — the outermost pixels are Android's system
      back gesture. Verified on the emulator; installed on the Pixel 8a. README updated.

- [x] 2026-07-04 — **Spoken error feedback.** Voice-reachable failures now speak a plain-language
      reason alongside the machine-readable `error`, instead of failing silently. New `spokenError`
      map (code → friendly phrase) + `conn.fail(code, msg)` helper that sends the `error` and, when
      the code is voice-reachable, a `say`; every client-facing `c.send(msgError(...))` routes
      through it (the job path emits the `say` before `finish` for `turn_failed`/`compress_failed`).
      Wire-level / programmer codes (`bad_message`, `bad_adopt`, `bad_delete`, `bad_rename`,
      `unauthorized`, `internal`) stay screen-only. `TestSpokenErrorFeedback`; docs/protocol.md +
      README updated.
- [x] 2026-07-04 — **Hands-free voice model verified on the Pixel 8a** end-to-end for the
      always-listening path (wake word → live draft → end-token commit → dictation).
- [x] 2026-07-04 — **Per-session naming by voice** (`rename` command). "hey buddy, rename to
      backend" / "rename this session backend" / "call this backend" renames the session you're
      **attached to** — no explicit old name, it targets the current session. New `command.Rename`
      Kind + registry entry + parse (anchors the new name after "to"/"session"/"this"; server
      `sanitizeName` collapses multi-word to one token). `doRenameCurrent` refuses when unattached /
      no name / same name / name taken, and speaks a confirmation on success; `doRename` now returns
      a success bool so the voice path only confirms on a real rename. Fully server-side (reuses the
      existing store rename + job re-key), so no new wire message. `TestParseRename`; commands.json
      regenerated; docs/commands.md + README updated.

- [x] 2026-07-04 — **Fix: history replay showed injected prompt scaffolding + duplicated a turn.**
      The server appends scaffolding to a dictation before sending it to Claude (brief-reply nudge,
      interactive-mode ask instruction, compress recap preamble) but echoes only the raw text live.
      History reads Claude's transcript, which stores the augmented prompt — so on reattach the
      injected text surfaced (never shown live), and because it no longer matched the clean live copy
      the app's `(role,text)` dedupe missed, leaving the turn duplicated/out of order. Now
      `serveHistory` runs user messages through `stripInjected` to recover the spoken text, so the
      history and live views are consistent and the replayed turn dedupes. Server-only.

- [x] 2026-07-04 — **Feat: persist the per-message token badge across reattach/restart.**
      The per-bubble context/cache badge was driven only by a live turn's `usage`, so on reattach or
      server restart the reloaded history came back badge-less (the transcript reader kept only text).
      `ReadTranscript` now also pulls each claude line's aggregate `usage`, attaching it to the
      **final** assistant line of a turn (matching the live closing-message badge, so a multi-line
      tool turn shows one badge, not several); `Message.usage` rides the `history` message and the app
      carries it into the chat bubble. So the badges are the same before and after a reload.

- [x] 2026-07-04 — **Feat: show context size immediately on attach (from the transcript).**
      The 🧠 title-bar readout was driven only by a live turn's `usage`, so after attaching it stayed
      blank until the first reply — no signal of what a `clear`/`compress` would reclaim. The server
      now reads the last assistant turn's aggregate `usage` (input + cache) straight from the on-disk
      transcript (`session.LastContextUsage`) and rides it on the `attached` message as `usage` +
      `usage_at` (that turn's unix time). The app seeds its context meter from it on attach and anchors
      the cache-warm countdown to the turn's real age, so a stale cache reads cold.

- [x] 2026-07-04 — **Fix: status-bar context-size readout didn't reset on `clear`/`compress`.**
      The title-bar 🧠 token count is driven by the last turn's `usage`, but `clear` (and `compress`)
      only rotated the session and spoke a `say` — neither told the app the context was now fresh, so
      the stale count lingered. Added a `context_reset` outbound message the server sends at both
      rotation points (`doClear` and the compress rotation in `startCompress`); the app drops its
      last-turn usage on receipt, so the readout returns to zero until the next dictation reports the
      true new size.

- [x] 2026-07-04 — **Fix: output produced while viewing another session was lost on switch-back.**
      A session keeps running while you view a different one, and its output is persisted to the
      transcript, but the server only fans live output to the currently-attached connection — so
      what it said while we were away never reached the app. The app fetched a session's history
      only on its **first** attach, so switching back never re-pulled the missed output. Now the app
      refetches recent history on **every** (re)attach and dedupes the top page against live messages
      already in the log (by role+text), so switching back to a busy session replays what it produced
      without duplicating what already streamed. (`VoiceController.kt`.)
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
- [x] 2026-07-04 — **Usage estimate: manual two-point rate benchmark (`Set`/`Calc` buttons).** The
      passive `/usage` calibration EMA-blends each reading and divides by a single, often-rounded
      percent delta, so the learned tokens-per-percent rate skews high and the estimate reads a few
      percent low — consistently, in the same direction. New `usage_set`/`usage_calc` messages +
      `Estimator.SetBenchmark`/`CalcBenchmark`: `Set` stamps the odometer + real percentages, then after
      burning enough tokens to move several whole percent `Calc` sets each window's rate **directly**
      from tokens/percent-gained (no EMA), so the multi-percent move drowns out the integer rounding.
      Sub-1% moves are refused. `bench_*` fields on `usage_estimate`; buttons + benchmark line in the
      app's usage sheet. `TestBenchmarkTwoPoint`.
- [x] 2026-07-04 — **Chat: don't snap to latest while scrolled up; add a jump-to-latest button.** A
      new message arriving mid-turn now auto-follows only when the reader is `pinned` — the END of the
      newest message is actually in view (the `snapshotFlow` at-bottom test tightened from "last item
      index in range" to "last item's `offset + size <= viewportEndOffset`"), so scrolling up even a
      little to read earlier text stops the yank. `LaunchedEffect(last)` is gated on `pinned`; the
      explicit `scrollTick` path (attach / typed send / read-last) re-pins. A round ↓ button overlays
      the bottom of `ChatList` (BottomCenter, above the status bars/input bar) while `!pinned`; tapping
      it re-pins and animates to the newest message. Built + installed on emulator and Pixel 8a.
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

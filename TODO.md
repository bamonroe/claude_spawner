# TODO — claude_spawner

The **live task list** for *open* work — anything unchecked or partially done. This is the single
source of truth for what's in flight; `README.md` keeps the historical phase-by-phase roadmap, and
**`FINISHED.md` is the archive of fully-completed work** so this file doesn't grow endlessly.

**Maintenance rule** (see `CLAUDE.md`): edit this file in the same commit that proposes or
completes a feature **or a test**. Adding a feature/test → add an unchecked box here. Finishing
one → check it off, dated. Dropping a test/feature → remove it with a one-line why. **Once a
feature or epic is fully checked off and confirmed done, move it out of this file into
`FINISHED.md`** — a partial epic keeps its done sub-items here for context and only migrates once
every box is checked. A change that leaves this file stale is incomplete.

Dates are `YYYY-MM-DD`.

## Active

- [ ] **Ephemeral frames vanish on full browser refresh + unattributed-frame leakage.** Root cause
      of a live-vs-refresh chat inconsistency the user hit while rapidly switching sessions with
      background job-notify turns firing. Two halves: (1) `say`/`error`/`activity`/`turn_stopped`
      frames are deliberately NOT written to Claude's transcript, so a full page reload (which wipes
      in-memory chat and rebuilds only from history) drops them — "shown live → gone on refresh",
      the mirror of the 2026-07-30 job-notify strip fix. (2) The client keying rule
      `keyOf(sessionId) = sessionId.ifEmpty { currentId }` files ANY unattributed frame under the
      currently-viewed session, so during a session switch an ephemeral frame lacking a `session_id`
      can surface in the wrong log and disappear on refresh. Content frames are all attributed +
      persisted (verified: no real message content was ever lost — both transcripts intact on disk);
      exposure is limited to ephemeral/dialog frames. Timestamps are NOT the cause (they're not
      mutated post-arrival). Decide the deep fix: guarantee every session-scoped ephemeral frame is
      attributed (extend `sessionScoped`/audit direct sends) so nothing leaks, and/or accept pure
      acks as ephemeral. Not yet fixed — needs a design call before a server rebuild.
- [ ] **Ping a still-connected phone on an eager notify.** When the eager turn fires to a session
      with no attached sink, push a lightweight `notice` frame to any device still connected but
      viewing another screen, so it surfaces the finished job without opening the session. Needs a
      new wire message (Protocol.kt + docsync) + app handling + an APK test cycle. (Truly waking a
      fully-closed/backgrounded app needs FCM push infra, which does not exist yet — separate epic.)
- [ ] **Unified versioned app↔server sync layer (one entry point, no drift).** Today every piece
      of shared state is reconciled ad-hoc: the four app-managed catalogues (hosts, identities,
      profiles, providers) and every per-client setting are **blind last-write-wins by name with no
      timestamp/version**, and the incoming/outgoing message handling is **written twice** —
      `VoiceController` (androidMain) and `WebAppController` (wasmJsMain) each hand-implement ~40
      apply branches + ~40 send sites independently, which has already drifted (web silently no-ops
      on `digests`, `session_list`, calibration, read-last). The `docsync`/`clientsync` drift tests
      enforce message-type and field-name parity but are blind to versioning/conflict semantics.
      Goal: **one authoritative, versioned sync point per side** that every syncable resource flows
      through, so a change is added in one place and both clients + the server stay consistent, and a
      newer server value (written by a *different* app client) reflects back to the app instead of
      being clobbered.

  - **Conflict model (chosen): per-record `updated_at` (client-stamped ms) + last-writer-wins with
    tombstones.** Every catalogue record and syncable setting carries an `updated_at`. The app is
    still source of truth on *first* write, but on any exchange the newer `updated_at` wins in either
    direction: the server keeps the max and rejects a stale upsert (echoing its newer record back so
    the app adopts it); the app adopts any incoming record newer than its local copy. Deletes become
    timestamped **tombstones** so a stale client can't resurrect a removed record. (Considered and
    rejected for now: server-assigned monotonic revision + optimistic-concurrency `base_rev` — more
    robust against clock skew but heavier; single-user/home-network makes wall-clock LWW enough.
    Revisit if clock trust becomes a problem.)

  - **Fast-path: per-catalogue digest (skip-if-equal).** Reconciling every record on every connect
    is wasteful when nothing changed — the common case after a long absence. Each side computes a
    **stable, order-independent checksum per catalogue** (e.g. XOR/sum of per-record hashes over
    `(key, updated_at, payload)`, so a timestamp-only change still flips it) and exchanges just that
    one small hash in the handshake. **Digests match → do nothing.** Only on mismatch does the side
    ship its (small) catalogue and let the LWW+timestamp merge resolve direction. This is exactly the
    `count`+`hash` freshness pattern the chat transcript already uses (`digests`/`history unchanged`),
    generalized to the catalogues. A flat per-catalogue checksum is enough here (tens of records);
    a Merkle tree — to binary-search *which* records differ without shipping the whole set — is only
    worth it if a catalogue ever grows large, so it's explicitly out of scope for now.

  - **Structure: a `commonMain` resource-sync registry.** Each syncable resource declares once —
    key function, `updated_at` accessor, and merge rule — in shared `commonMain` code. A single
    inbound `applyServerState(msg)` and outbound `pushLocalChange(resource, record)` dispatch by
    resource, replacing the duplicated per-controller `when` bodies; `VoiceController`/`WebAppController`
    keep only the thin platform bits (StateFlow wiring, disk cache). The server gets the symmetric
    single apply/broadcast entry point with the LWW+tombstone arbitration, replacing the four
    near-identical `do*Put`/`flush`/`broadcast` paths in `gateway/{hosts,identities,profiles,providers}.go`.

  - **Phasing** (versioning is a wire change → land coordinated across both worktrees, drift test first):
    - [x] **Phase 1 — pure refactor, no wire change (app worktree). DONE** (branch `app`). The
          duplicated inbound apply + outbound send logic is now hoisted into shared `commonMain`
          reconcilers so the two controllers can't drift; web brought to parity; `dedupeCachedLog`
          moved onto server `index`. Drift tests stay green (no new messages). Slices below.
          - [x] **Slice 1 (catalogues) — done, branch `app` `acea2f4`.** New
                `commonMain/net/CatalogueSync.kt`: a `Catalogue<T>` (StateFlow + `key` + `merge` seam
                for Phase 2's LWW/digest) and a `CatalogueSync` owning all four catalogues (hosts,
                identities, profiles, providers) with one `apply(msg)` inbound + all outbound mutators.
                `VoiceController`/`WebAppController` lost their four StateFlow holders, four inbound
                branches, and 13 mutator bodies — now thin delegations. Both targets compile.
          - [x] **Slice 2 (web parity) — done, branch `app` `f620173`.** Web `onMessage` was one
                collapsed `else -> {}` swallowing six variants; now exhaustive over all 41 `ServerMsg`
                (compile-time guard like Android). Implemented `digests` cache-validation on web (sends
                `digest` on `HelloOk`, stores per-session `(count,hash)`, `requestFreshHistory()` skips
                refetch when held==server, handles `unchanged` — fixed a latent bug where an empty
                `unchanged` page wiped the log), `read_last`, and `pending` (real web VAD). Documented
                intentional no-ops for `calibration`/`dialog` (Android-hardware / already-via-`say`).
                Note: `session_list` from the old map doesn't exist in the protocol — nothing to do.
          - [x] **Slice 3 (session/chat reconcile) — done, branch `app` `0d90713`.** New
                `commonMain/net/SessionSync.kt` (sibling to `CatalogueSync`) owns the reconcile
                DECISION logic; a 7-accessor `SessionSync.Host` seam keeps platform side effects local
                (Android disk `TranscriptCache` + timestamped merge + reconnect gap-fill; web in-memory
                index-sorted merge). Unified `attached`/`detached`/`context_reset`/`renamed`/
                `discovered`/`history`/`digests`/`swap`/`focusKnownSession`. Single `SessionSync.dedupe`
                (index-first, `(role,text)` fallback only to drop a live `index==-1` row once its
                indexed row lands) replaced both the Android `dedupeCachedLog` and the web inline
                `distinctBy`. Both targets build; no wire change.
                - [ ] **Follow-up (deferred, optional):** history-merge algorithm left per-client by
                      design (Android timestamp+gap-fill vs web index-replace) — only the dedup
                      primitive is shared; unifying would be a behavior change. (The web
                      `renamed`/`discovered` digest-key / title gap noted here was closed 2026-07-17 by
                      the mutation-message audit above.) Revisit if full history-merge parity is wanted.
          - [x] 2026-07-29 — **On-device verification:** confirmed in the field on the live Pixel 8a
                self-hosting client — the sporadic duplicate rows have not recurred in a long time of
                daily use (see the duplicate-rows epic above; the bug was never deterministically
                reproducible, so live-use absence is the verification).
    - [x] 2026-07-30 — **Phase 2 — add `updated_at` to the catalogue records + messages.** Extend
          `Host`/`Identity`/`ProfileInfo`/`AgentInfo` and their Go structs with `updated_at`; server
          persists it and arbitrates LWW; add tombstones for deletes. Done: `UpdatedAt` on all structs,
          LWW/tombstone arbitration in `hosts.go`/`profile.go`/`identity.go` (+ `tombstone.go`, `lww_test.go`),
          wire carries `updated_at` both directions. The "coordinate/merge across worktrees" caveat is moot —
          the tree folded back to a single `master`. (verified against code)
          - [x] **Phase 2a (LWW + tombstones) — done, master.** Landed coordinated on `master`
                (Go + Kotlin together). Each record carries client-stamped `updated_at` (unix ms);
                server rejects older upserts + re-broadcasts current; timestamped tombstones block
                stale re-adds (new `session/tombstone.go`); app stamps edits via a new expect/actual
                `nowEpochMs` clock and `CatalogueSync` merges LWW by `updatedAt`. `docs/protocol.md`
                updated; Go arbitration unit tests added; `go test -count=1` + both APK/wasm builds
                green. Still needs **master→app merge** and on-device check.
          - [x] **Phase 2b — per-catalogue digest fast-path (skip-if-equal) — done, master (uncommitted
                at hand-off).** Each side folds a catalogue's live records `(key, updated_at, payload)`
                into one order-independent checksum (64-bit FNV-1a per record, wrapping-summed, hex —
                portable so Go `gateway/catalogdigest.go` and Kotlin `net/CatalogueDigest.kt` compute
                byte-identical values, cross-verified). The app presents all four digests in `hello`
                (fresh each send, via a `SpawnerClient` provider — catalogues aren't persisted, so a
                cold start yields empty digests → safe full re-broadcast); the server suppresses each
                catalogue's connect-time send when the digest matches, else broadcasts as before and
                lets the Phase 2a LWW merge resolve direction. Hosts/identities, previously request-only,
                now reconcile proactively on connect too (only when they differ). Tombstones aren't
                folded in (a pending delete shows as an extra record → digests already differ). Go unit
                tests: fold order-independence + add/change/delete/timestamp flips, connect-suppress vs
                connect-broadcast. `docs/protocol.md` updated; `go test ./... -count=1` + both APK/wasm
                builds green. Still needs commit + master→app merge.
          - [x] **Note (closed):** `fieldsync`/`docsync` originally guarded docs↔Go only and could not
                catch a missing *Kotlin* field. Now `recordsync_test.go` reads the Kotlin parsers and
                asserts record field-parity (incl. `updated_at`) both ways for every catalogue, and Phase
                4's `TestCatalogueRegistryComplete` makes that coverage total — so the drift test now
                catches a missing Kotlin field, not just the build.
    - [x] **Phase 3 — route genuinely-shared server-global settings through the sync layer — done,
          master (uncommitted at hand-off) 2026-07-16.** Added a fifth catalogue, **settings**: keyed
          `{key, value, updated_at}` records (value always a string, typed at the edges) with per-key
          LWW + tombstones (`session.SettingKV`, persisted to `settings_kv.json`) and an order-independent
          FNV-1a-64 digest byte-identical to Kotlin (`settingsDigest` ↔ `CatalogueDigest.settings`, pinned
          by a known-hex cross test). Six keys routed: `whisper_model`, `whisper_fast_model`,
          `warm_compress`, `auto_compress`, `auto_compress_threshold`, `summary_only` — so the whisper
          model (was server-global-mutable), auto-compress (was in-memory-only, clobbered on every hello),
          and summary-only (was stateless) now persist and propagate to every client with the same LWW
          rule. New wire messages `setting_put` (in) / `settings` (out) + `settings_digest` in `hello`;
          `hello` no longer clobbers `acCfg`. App: `Catalogue<SettingRecord>` in `CatalogueSync`, the
          picker/switches call the synced mutator and inbound `settings` mirrors back. Per-device prefs
          (wake sensitivity, on-device-vs-server TTS, color scheme) stay LOCAL — out of scope. Whisper
          keeps its dedicated `set_whisper_model` load message (which triggers the resident-server
          load/download); the server persists the result into the catalogue on success. `docs/protocol.md`
          updated; `go test ./... -count=1` + wasm/APK builds green. Still needs commit + master→app merge.
    - [x] 2026-07-29 — **Phase 4 — drift test asserts every registered syncable resource carries a
          version token.** The per-catalogue field-parity check (`recordsync_test.go`) already asserted
          `updated_at` on both the Go wire record and the Kotlin parser for the six catalogues, but off a
          hand-maintained `rows` list — so a *newly-registered* catalogue could skip the check. Closed the
          meta-gap: new `TestCatalogueRegistryComplete` treats the **digest layer as the authoritative
          registry** (a synced catalogue must have a digest on both ends for the skip-if-equal fast path)
          and requires the Go digests (`catalogdigest.go` `<catalogue>Digest` funcs), the Kotlin digests
          (`CatalogueDigest.kt` public folds), and the parity rows to be the **same set** — so registering
          a catalogue anywhere without a parity row (and thus without the both-ends `updated_at` assertion)
          fails the build. Each digest func is also asserted to fold the version token (Go `UpdatedAt`
          selector / Kotlin `updatedAt`), so a digest can't be defined without the conflict rule. `rows`
          hoisted to a reusable `catalogueRows(t)` helper. Negative-tested (corrupt a row name → four
          actionable failures); `go test ./internal/docsync/ -count=1` green. This completes the unified
          versioned sync-layer epic.

- [x] 2026-07-17 — **Context handoff across backend switch.** Switching a session's AI (`set_agent`,
      `doSetAgent` in `gateway/ops.go`) still rotates to a fresh `session_id` (Claude/Codex/opencode
      transcripts are incompatible on disk), but no longer drops the conversation: before rotating it
      reads the outgoing backend's transcript via the existing **generic** `transcriptReader`
      (`Driver.ReadTranscriptChain`) and stashes a portable, recent-weighted, neutrally-labeled recap
      (`formatHandoffRecap`, budget-bounded) as `PendingSeed`. The existing compress seed path
      (`dictate` → `seedPreamble`) then injects it into the new backend's first turn, so any backend
      whose transcript is readable hands off to any other with zero per-backend code. A backend with a
      null transcript (antigravity today) yields an empty recap and switches clean as before. Unit
      tests in `gateway/handoff_test.go`; full `go test ./...` green. Not yet confirmed on-device.
  - [x] 2026-07-17 — **Server side DONE (segmented display history).** Shipped: `Session.History
        []HistorySegment{Agent,Host,IDs}` (durable, omitempty → old sessions unchanged); `doSetAgent`
        archives the outgoing backend (`Driver.ArchiveSegment`, skipped for an un-run backend) before the
        rotation nulls `PriorIDs`; display (`serveHistory`, `serveDigests`) now reads
        `Driver.ReadDisplayHistory` — each archived segment via its own backend reader, then the current
        chain, concat + globally re-indexed for stable pagination; full delete uses `Driver.DeleteSessionAll`
        so a switched-away backend's transcripts don't orphan; CONTEXT/usage reads stay current-backend
        (unchanged). Antigravity convergence in place: `currentHistoryIDs`/`ArchiveSegment` use `AgyBrainIDs`
        for agy. Tests in `session/history_display_test.go`; full `go test ./... -count=1` green. **Remaining
        (app worktree): reword/drop the now-false "this deletes history" warning in the switch dialog** — the
        server keeps the transcript and carries a recap, so nothing is deleted. Scrollback needs NO app change
        (server serves one merged list as before). Not yet confirmed on-device.
  - [ ] ~~**Follow-up: preserve the OLD backend's messages in the display log across a switch (server-owned,
        drift-free).**~~ *(design below — implemented above)* Live-tested finding: after a Codex→Claude switch the app's chat log goes blank —
        you can't scroll back to the Codex messages — and the app still shows a "this will delete history"
        warning that is now false (the server keeps the transcript; it carries a recap forward). Fix both
        **server-side** so the app just renders what the server serves (no app-local message memory → no
        app/server drift). Design:
    - **Root constraint (reshapes the naive fix).** Display history now spans *multiple backends*, and
      `ReadTranscriptChain(agentID, host, ids)` is **monomorphic** — it reads every id with the *one*
      current backend's reader (`session.go:507`). So we cannot just keep the old Codex id in `PriorIDs`:
      after the switch the Claude reader would try to parse a Codex transcript and get nothing. The
      display history must be a **segmented list tagged with the backend (+host) that wrote each segment**,
      each segment read by its own reader and concatenated.
    - **New field** `Session.History []HistorySegment` where `HistorySegment{ Agent, Host string; IDs []string }`
      (durable JSON, rides clear/compress like `PriorIDs`). `PriorIDs` stays the *current-backend*
      rotation chain (clear/compress); `History` holds the *archived previous-backend* segments.
    - **Split the three consumer groups of `TranscriptIDs()`** (all conflated today):
      **DISPLAY** (`serveHistory` ops.go:506, `serveDigests` ops.go:544, and the handoff recap read
      ops.go:369) → read each `History` segment with `ReadTranscriptChain(seg.Agent, seg.Host, seg.IDs)`,
      concat, then append the current chain, re-index globally for stable pagination.
      **CONTEXT/usage** (attach badge ops.go:670/698, turn-complete jobs.go:314, autocompress.go:85) →
      **unchanged**, stays current-backend `TranscriptIDs()` (usage must reflect the live backend; note
      `lastContextUsage` already walks newest-first so it only ever reads the current id in practice).
      **DELETE** (`doDeleteDiscovered` ops.go:473, `doClear` purge ops.go:1022) → full-session delete must
      **also** enumerate `History` segments and call each backend's `deleteByIDs`, else a switched-away
      transcript is orphaned on disk forever.
    - **`doSetAgent` change** (ops.go:384): instead of `rec.PriorIDs = nil` discarding the old chain, push
      `HistorySegment{oldAgent, oldHost, TranscriptIDs()}` onto `rec.History`, THEN reset `PriorIDs=nil`
      and rotate. The recap-into-context path is untouched — this only preserves the old messages for
      *display/delete*, orthogonal to what the new backend reads.
    - **No protocol/app change for the display fix** (the drift win): `serveHistory` still emits one flat
      `messages` list built server-side, so the app renders the merged log unchanged. The only app-side
      edit is the stale warning copy in the switch dialog — reword to "context is carried over, history is
      kept" (or drop it); coordinate that one string in the `app` worktree.
    - **Converges with antigravity.** An antigravity `History` segment's `IDs` are its `AgyBrainIDs`, read
      by the future `antigravityFS` reader — the same backend-tagged-segment seam the "Next: wire the
      `antigravityFS` reader" item below needs. Build this split and that reader together.
    - **Back-compat:** existing sessions have no `History` (omitempty) → behaves exactly as today. Optional
      nicety: `store.GetByAnyID` (store.go:178) could also scan `History` ids so attach-by-old-id resolves.

- [x] 2026-07-15 — **Antigravity (`agy`) backend.** Added Google's Gemini-powered `agy` CLI as the
      fourth AI backend (`server/internal/agent/antigravity.go`), registered in `agent.Default()`.
      Driven non-interactively via `agy --prompt` (its only headless mode — verified live: no
      machine-readable stream, plain-prose stdout is the reply). Caller-supplied `--conversation`
      uuid (created then resumed, like Claude, so `SelfAssignsID` false — verified resume recalls
      prior context). agy ignores the process cwd, so a new `TurnSpec.Dir` threads the session
      directory into `--add-dir`. Per-target binaries `SPAWNER_SSH_AGY_BIN` / `SPAWNER_SANDBOX_AGY_BIN`.
      Gemini 3.x Pro/Flash model catalogue with spoken aliases. Its on-disk store isn't wired to a
      reader (keyed by an internal id we don't hold, no usage recorded), so it routes to a new
      `nullTranscript` reader — reply streams live, but no history replay/context/deletion. Unit tests
      for args + parser; full `go test ./...` green.
  - [x] 2026-07-17 — **Antigravity reply paragraph reconstruction.** `agy --print` concatenates a
        turn's several assistant messages into one blank-line-less blob on stdout, so multi-message
        turns rendered as a single wall-of-text paragraph in the app. After each antigravity turn the
        driver now reads agy's on-disk `brain/<id>/…/transcript.jsonl`, pulls the ordered
        `PLANNER_RESPONSE` messages, and rejoins them with blank lines
        (`session/antigravity_transcript.go`, `reconstructAgyReply`). Since agy ignores our
        `--conversation` id (see below), the right transcript is found by content-matching the newest
        few brain transcripts against the stdout blob — which also guards the rewrite: only line breaks
        change, never wording, and it falls back to the stdout reply on any miss. Unit tests + full
        `go test ./...` green.
  - [ ] **Follow-up: agy ignores `--conversation` → turns don't actually resume.** Recent `agy` logs
        "conversation … not found, ignoring --conversation flag" and creates a fresh conversation
        (keyed by its own internal id) every turn, so an antigravity session has NO cross-turn memory
        despite the code passing our uuid. Fix by capturing the id agy creates (e.g. from a pinned
        `--log-file`'s `Print mode: conversation=<id>` line) and passing THAT back as `--conversation`
        next turn — which would also give a stable id to key a real history reader on. The comments in
        `antigravity.go` are corrected to say resume is a no-op today.
  - [x] 2026-07-29 — **Follow-up: real antigravity history reader.** Done — `TranscriptAntigravity`
        now routes to `antigravityFS` (below), so reattach replays agy history. (It was never actually
        blocked on the `--conversation` resume bug: the brain-id capture foundation gave us the stable
        our-session → agy-brain mapping the reader needed, independent of whether agy resumes.)
    - [x] 2026-07-17 — **Foundation: capture the per-turn brain id (the missing mapping).** Since agy
          ignores our `--conversation` id and files every turn under a fresh internal brain id, we now
          build the mapping ourselves: `agyBrainScript` emits each transcript's path, `matchAgyParagraphs`
          returns the matched block's brain id, and `Driver.Turn` records it in turn order on the new
          `Session.AgyBrainIDs` field (deduped against the last, rides through clear/compress). Read path
          unchanged (still `nullTranscript`), so zero regression — this just starts recording the data the
          reader needs. Unit tests in `antigravity_transcript_test.go`. Needs live agy turns to confirm
          the ids populate on real hardware.
    - [x] 2026-07-29 — **Wired the `antigravityFS` reader over `AgyBrainIDs`.** Given a session's
          `AgyBrainIDs`, it reads each `brain/<id>/…/transcript.jsonl` and assembles `[]Message`
          (USER_INPUT → user turn, unwrapped from the `<USER_REQUEST>` envelope; PLANNER_RESPONSE steps →
          one joined reply, mirroring `reconstructAgyReply`). The interface seam was already solved:
          `currentHistoryIDs`/`transcriptReaderFor` route `AgyBrainIDs` + host to the reader (the
          backend-aware `HistoryIDs()` this item anticipated already existed). `deleteByIDs` is a no-op
          (brain dirs are agy's store, not ours to reap) and `lastContextUsage` returns nil (agy has no
          token counts). Durable brain-scoped row ids; `nullTranscript` removed; docs updated.
  - [ ] **Follow-up (gated on Google): rich agy turns via JSON output.** When `agy` grows a
        `--output-format json`/stream mode (or a resolvable transcript path keyed by our conversation
        id), replace `parseAgyText` with a real stream parser and wire an antigravity transcript
        reader, to recover live tool breadcrumbs, **token/context accounting**, and reattach history —
        the three things the plain-text v1 can't do. Today agy exposes no token counts anywhere.

- [ ] **Verify + re-apply the audio output route after selecting it (Bluetooth/car stickiness).**
      `AudioRouter.setOutput` calls `setCommunicationDevice` fire-and-forget and returns its raw
      boolean; `setAudioOutput` then commits `_audioOutput.value` even if the platform silently
      reverted the grab (common with car BT stacks, which fall back to A2DP with no comm route). So
      the app's state and the actual route diverge and the user has to re-pick a few times before it
      "takes" (observed live: paired in the car, had to toggle output back and forth before speaker
      engaged). Fix: after `setOutput`, re-read `AudioRouter.current()`/`communicationDevice` and, if
      it didn't land on the requested device, retry (poll a short window like the SCO-mic
      `verifyHeadsetMic` path already does) before trusting the selection — and surface a transient
      state if it never engages. Needs real in-car BT testing to validate; deferred from the
      2026-07-14 hands-free mic fixes. See `VoiceController.setAudioOutput` / `AudioRouter.setOutput`.
      Code-side fix landed 2026-07-16: route picks now retry for ~6 s and commit prefs/UI only after
      `AudioRouter.outputActive` confirms the route; failed picks restore the previous route and show
      a transient "audio route unavailable" status. Clean `:app:assembleDebug` green and installed on
      the Pixel 8a. Still open for real in-car Bluetooth validation.

- [ ] 2026-07-13 — **Providers settings tab (Settings → Providers).** A per-backend settings overlay
      that mirrors Profiles: pick the model a fresh spawn defaults to, and toggle which models the
      voice `list models`/`use model N` commands enumerate. Backends stay compile-time; only the
      overrides are stored.
      - [x] Server layer: `agent.SettingsStore` (`SPAWNER_PROVIDERS`/`providers.json`, validated
        against the registry, nil-safe reads), driver `Providers` field + `ProviderSettings()`, the
        `provider_put` wire handler (`bad_provider`), enriched `agents` message (effective default +
        per-model `voice` flag, re-broadcast on change), spawn default-model stamping + voice-command
        filtering now honor the overlay. Kotlin `AgentInfo.voiceModels` + `providerPut` builder.
        Docs + drift tests green.
      - [ ] Client tab: `ProvidersController` + `ProvidersSettings` composable, `SettingsHub` row,
        `MainActivity`/`WebRoot` nav branches, controller impls, APK build + phone install.

- [ ] **Per-session record locking** (1.0 quality pass, deferred item). The store hands out
      shared `*session.Session` pointers; a running turn's goroutine mutates the record
      (Started/PendingSeed/primes + Put) while another device's read loop can mutate it too
      (kill-job, set_agent/model, rename). The one-writer invariant covers turn-vs-turn and
      reconcile, but not turn-vs-ops or two devices. Wants a deliberate design (per-record
      mutex or store-mediated mutation), not a drive-by — scoped out of the 2026-07-13
      concurrency fixes below.

### Dedicated wake-word / end-token detector via LiveKit (epic — proposed 2026-07-14)

Replace Whisper-as-keyword-spotter with a purpose-built keyword-spotting model. Today all live
hands-free detection funnels through `gatedChunk` (`internal/gateway/stream.go`): each VAD clip is
fast-transcribed by the resident Whisper server and the wake word ("hey buddy") and end token are
found by **string-matching the transcript** — backed by a hand-curated list of Whisper mishearings
(`command.wakePhrases`) that also causes false triggers. That's the wrong tool: Whisper is a full
transcription model, and on the live deploy it's the **heavy `large-v3`** doing double duty (detection
on tiny clips + accurate transcription), which is both unreliable on short clips and wasteful.

**Decision — going with LiveKit (`livekit/livekit-wakeword`), not openWakeWord.** Both are Apache-2.0.
LiveKit replaces openWakeWord's simple classifier with a conv-attention model (claims ~100× fewer
false positives), ships a **native Rust runtime crate** (mel + embedding compiled into the binary,
loads only the small classifier `.onnx` at runtime — no Python), and a model we train ourselves on
synthetic Piper TTS data is commercially unrestricted (the CC-BY-NC restriction is only on
openWakeWord's *distributed* pre-trained models, not on self-trained weights or the Apache-2.0 Google
embedding backbone). Its flagship conv-attention ONNX is **not** openWakeWord-loadable (only its
fallback `dnn`→TFLite head is), so we commit to LiveKit's own runtime end-to-end rather than straddle.
Accurate full transcription on commit stays on Whisper, untouched.

- [x] 2026-07-15 — **Offline trainer (one-time).** Docker trainer image (bare-metal abandoned:
      numba/llvmlite vs Python 3.14). Tokens are **doubled monosyllables**: START = "bump bump"
      (wake), END = "beep beep" (end); soft dropped-consonant forms are positives, each token lists
      the other as negatives. Full `run configs/bump.yaml` (conv_attention/medium, 20k samples, 50k
      steps) → `output/bump_bump/bump_bump.onnx`, **eval recall 99.7%, 0 FPPH over ~19h** (optimal
      threshold ~0.07). Augmentation was parallelized across CPU cores (patch) to survive the 4-core
      host. `beep.yaml` **done 2026-07-15** → `output/beep_beep/beep_beep.onnx`, **eval recall 99.8%,
      0 FPPH over ~19h** (optimal threshold ~0.04). Both tokens now trained + validated. Configs
      tracked in `wakeword/configs/`.
- [x] 2026-07-15 — **Rust sidecar service (the new component).** `wakeword/service/` (axum,
      pure-Rust `ort-tract`), image `spawner-wakeword:latest`. `GET /health`, `POST /detect` (LE i16
      mono PCM → per-model scores). Env: `WAKEWORD_ADDR`, `WAKEWORD_MODELS` (comma `name=path`),
      `WAKEWORD_INPUT_RATE`. **Verified with the real bump_bump model**: a 2s window ending at the
      wake word scores ~0.98, real background scores ~0.003. New `SPAWNER_WAKEWORD_URL` config still
      to be wired into `internal/config` + gateway (empty = disabled, fall back to Whisper).
- [x] 2026-07-15 — **Live streaming path (`GET /stream`).** Decided (over the abandoned ort-backend
      swap) to lean into the crate's streaming design instead of re-slicing whole clips: a persistent
      WebSocket takes continuous PCM and keeps a rolling `WAKEWORD_WINDOW_SEC` buffer, re-scoring the
      live edge every `WAKEWORD_HOP_SEC` of fresh audio. **Bounded ~one-window cost per score
      regardless of utterance length** (release: a 2s window ≈ **360 ms** idle; whole-clip slide was
      already 1.4s at 4s and climbs). Verified streaming a pre-filled buffer: token peaks ~0.985,
      background ~0.003. Key correctness fix: a live stream must **not** silence-pad a partial buffer
      (that fabricated ~0.57 on background) — it waits out the first full window, then scores real
      audio only. `axum` gains the `ws` feature. Fires the instant "bump bump" completes, before the
      command is even spoken — the latency win Whisper-on-closed-clips can't match.
- [x] 2026-07-15 — **Detector-model TRAINING removed from this repo (delegated out-of-tree).**
      Model training is orthogonal to the app: claude_spawner only needs a working wake/end-token
      model and the runtime that *consumes* it. So the in-app training-data capture was ripped out —
      the `train_clip`/`train_saved` wire messages, the server save path (`startTrainClip`/
      `saveTrainClip`/`slugify`, `SPAWNER_WAKEWORD_TRAIN_DIR`, the docker-compose real-dir mount), and
      the Android capture UI (`TrainingState`/`TrainPhase`/`TrainPrompt`, `TrainingDialog`, the
      "Add live training data" button, the `fixedMs` fixed-length recorder branch). Building /
      augmenting / retraining the models — including real-voice collection to close the
      synthetic-to-real gap — now lives in a **separate training project at `/data/livekit_training`**
      (the `run_copy_real` real-clip ingestion built earlier lives with the trainer, not here). The
      **runtime consumer stays**: the `spawner-wakeword` sidecar, `SPAWNER_WAKEWORD_URL`/`_THRESHOLD`,
      `internal/detect`, and the `gatedChunk`/`endTokenFired` detection path (next item).
- [x] 2026-07-15 — **Gateway swap (Go): end-token detector wired.** New `internal/detect` package
      (`Detector` interface + `RemoteWakeword` HTTP client POSTing raw PCM16LE 16k to `/detect`,
      unit-tested). `gatedChunk` now gates the end-token commit on the detector when
      `SPAWNER_WAKEWORD_URL` is set (`conn.endTokenFired`, thresholded by `SPAWNER_WAKEWORD_THRESHOLD`
      default 0.5), falling back to the Whisper string-match on nil/error — the A/B safety net.
      `commitMessage` (accurate parse) untouched; fast transcript still drives the live draft +
      barge-in. Build/vet/full `go test` green. Still TODO: (a) live A/B on the phone to confirm it
      beats Whisper on real misses; (b) extend the `calibrate` path to report detector scores so the
      threshold is field-tunable; (c) retire `command.wakePhrases` once the detector is trusted. The
      DESIGN (kept for the record): the detector is a **per-clip gate**, not a
      transcription replacement. Both audio paths (push-to-talk *and* hands-free VAD) hand the server a
      **bounded clip** — there is **no always-on mic and no continuous stream** (so `/stream` and option
      (b) are NOT needed here; `/stream` stays for a possible future mid-word latency play only). The
      wake word is never spoken alone — it's always wake+command, ~almost always > 2s, and the rare
      short clip is padded — so `POST /detect` (padded, slides) is the right endpoint. The detector
      answers two yes/no questions on the clip: (1) is a command starting (`bump_bump`), and — the
      high-value one — (2) **was the end token spoken (`beep_beep`)**. The end token is exactly where
      Whisper string-match gives the **false negatives** today. When the detector fires the end token,
      **hand the whole accumulated utterance to accurate Whisper** (`commitMessage`, unchanged) to
      parse the command and everything before it. So the detector does NOT produce command text — it
      only decides *when to call Whisper* — which also dissolves the earlier "barge-in needs the word
      after the wake" wrinkle (Whisper still does all real transcription). Plan: new `internal/detect`
      package (`Detector` interface + `RemoteWakeword` HTTP client POSTing raw PCM16LE 16k to
      `/detect`), `SPAWNER_WAKEWORD_THRESHOLD` (default 0.5; optimal ~0.04–0.07), thresholding in Go,
      wired into `gatedChunk` behind `SPAWNER_WAKEWORD_URL` with **graceful fallback to today's
      Whisper string-match** on nil/error (the A/B safety net). Keep the fast transcript for the live
      draft. Riskiest bits: threshold calibration (10× gap default vs optimal — extend the `calibrate`
      path to report detector scores) and per-clip vs accumulated end-token semantics.
- [x] 2026-07-15 — **Per-client wake/end-token backend toggle (default Whisper).** The detector is
      no longer forced on every client when `SPAWNER_WAKEWORD_URL` is set. A new `hello` field
      `wake_service` (`whisper` default | `detector`) is stored per-`conn` (`c.wakeService`); the
      detector is only scored in `endTokenFired` when the client opted into `detector`, otherwise (and
      for older/empty clients) the always-present Whisper string-match runs — with the same graceful
      fallback if `detector` is chosen but no sidecar is configured or it errors. Since we don't yet
      trust the trained LiveKit model live, Whisper stays the default so hands-free is guaranteed to
      work. Server + protocol.md + `TestEndTokenFired` cases done on the server worktree. **App half
      done (app worktree):** persisted `Prefs.wakeService` (default `"whisper"`) in `SettingsStore` +
      `WebPrefs`, a "Wake/end-token detection" switch in the Commands settings screen (off = Whisper
      default, on = dedicated detector), threaded through `HelloConfig`/`Outbound.hello`
      (`put("wake_service", …)`) from both the Android `VoiceController` and the web `WebAppController`.
      Clean-built and installed on the Pixel 8a (launch verified — no stale-dex crash from the grown
      ctor).
- [ ] **Positional detection → correct Whisper with the detector (proposed 2026-07-15).** The
      detector knows a token was said far more reliably than Whisper; use *where* it fired to stop
      Whisper's mishearings from corrupting the parse. Enabling change: the sidecar returns the token's
      **time offset** (the peak window's position — `predict_peak` already computes the argmax window,
      just surface it) alongside the score, on both `/detect` and `/stream`. Then in `commitMessage`:
      instead of string-searching the accurate transcript for the end token (which fails when Whisper
      mishears "beep beep"), **trim the audio at the detected offset before the accurate pass** so
      Whisper only ever transcribes the real command and can't mis-splice the token. Generalizes:
      request word-level timestamps from Whisper, align them against the detector's high-confidence
      token positions, and where they disagree **trust the detector** (force the token in/out at that
      spot) — also covers mid-utterance wake tokens once the sidecar can report *all* above-threshold
      window positions, not just the peak. Do this AFTER the base end-token gate is proven live (below),
      so we're not stacking on an unverified layer.
- [x] 2026-07-15 — **Per-client wake/end-token backend toggle (default Whisper).** The detector is
      no longer forced on every client when `SPAWNER_WAKEWORD_URL` is set. A new `hello` field
      `wake_service` (`whisper` default | `detector`) is stored per-`conn` (`c.wakeService`); the
      detector is only scored in `endTokenFired` when the client opted into `detector`, otherwise (and
      for older/empty clients) the always-present Whisper string-match runs — with the same graceful
      fallback if `detector` is chosen but no sidecar is configured or it errors. Since we don't yet
      trust the trained LiveKit model live, Whisper stays the default so hands-free is guaranteed to
      work. Server + protocol.md + `TestEndTokenFired` cases done here; the app Settings toggle
      (Prefs + SettingsScreens + VoiceController hello) is the app-worktree half.
- [ ] **Go-live plumbing + live A/B (the actual next functional step).** Run `spawner-wakeword` as a
      **resident service** (compose service with both `bump_bump` + `beep_beep` models mounted), set
      `SPAWNER_WAKEWORD_URL` (+ optional `SPAWNER_WAKEWORD_THRESHOLD`) in the deploy env, restart the
      server, and compare hands-free end-token catch rate against today's Whisper string-match on real
      phone audio. The restart kills the in-flight turn, so do it on a scratch port or when ready to
      drop the session. This is the gate for both trusting the detector and the positional work above.
- [x] 2026-07-30 — **Docs:** `README.md` (setup: training a model + running the detector service),
      `docs/architecture.md` (the detector seam + data flow), config vars (`SPAWNER_WAKEWORD_URL`/
      `_THRESHOLD` in `docs/config.md`, the authoritative home the `CLAUDE.md` config section now points
      to) all done; added the **`command.wakePhrases` retirement path** note to `docs/commands.md`
      (drop the mishearing list once the detector is trusted and made the default).

**Open decisions (resolve before coding):**
- **Frames vs. clips — RESOLVED empirically (2026-07-15): must slide the window.** The classifier
  scores the **last 16 embeddings ≈ the last 2s**, and training places the wake word at the **tail**
  of that window. Verified: "bump bump" at the *start* of a longer clip with a command following
  scores ~0.003 (misses); the same word at the *tail* of a 2s window scores ~0.98. So the gateway
  **cannot** hand the sidecar one big "wake word + command" clip. It must score a **sliding 2s
  window** over incoming audio and fire the instant a window ending at "now" crosses threshold —
  which naturally happens right when "bump bump" completes, before the command is spoken.
- **Sidecar-side sliding + cost model — benchmarked 2026-07-15 (numbers under 4-core contention from
  the beep training; idle will be lower).** The crate's `predict` is **stateless**: it recomputes the
  mel + **every** 76-frame/stride-8 embedding over the whole chunk each call, then keeps only the last
  16. The embedding-net forward pass is the cost — **~13–20 ms per embedding**; model construction is
  only ~50 ms (measured via a sub-window clip that early-returns). Per-call latency is **linear in
  clip length**: 2s≈383ms, 4s≈700ms, 10s≈1650ms (contended). Implications for "fires every utterance":
  1. **Do the sliding in the sidecar, not the gateway** — the gateway just POSTs the clip and gets a
     peak score (already implemented as a first cut: slide a 2s window in HOP_SEC steps, max score).
  2. **BUT the naive version I shipped re-runs `predict` per window → recomputes overlapping
     embeddings → N× blowup.** The fix: a single `predict` over the whole clip *already* computes all
     embeddings and discards all but the last 16. `MelspectrogramModel` + `EmbeddingModel` are public,
     so compute embeddings **once** over the clip, then slide only the tiny classifier over each
     consecutive 16-embedding window (own `ort` Session per classifier). Net: the full window sweep
     costs ≈ **one** detection, not N. This is the real implementation of the sidecar.
  3. **Hold the model resident** (construct once at startup, share behind a mutex / small pool) to
     drop the ~50 ms per-call construction and avoid re-reading the ONNX every request.
  A short isolated "bump bump" VAD clip (~0.7s, padded to 2s) is then one embedding pass ≈ ~150 ms
  idle — fine for wake latency; a long "bump bump <command>" clip sweeps for ~one embedding pass too.
- **Custom end token.** A trained model only knows its trained phrase. To keep the end token
  user-configurable, either train a small set of preset tokens or keep the Whisper string-match as the
  fallback for arbitrary tokens.

### SSH-native unified execution (epic — proposed 2026-07-08; foundation landed 2026-07-08)

**Why:** collapse the three execution paths (host fork, sandbox `podman exec`, would-be remote) into
**one SSH transport**. Every turn — including on the local machine — runs over SSH, so localhost is
just another host in the pool and there's a single code path to maintain. This also lets us
**containerize the server again without a root broker**: instead of a bespoke privileged host agent
(the thing we tore out in the 2026-07-06 revert), the container SSHes into the "real" host exactly as
it would any remote box, leaning on SSH's battle-tested auth/encryption/signal-delivery instead of
inventing our own. Motivated by wanting to drive Claude on the work box (`ssh work` → `potato`, has
`claude` + `podman`).

**Design (worked out 2026-07-08):**
- **Native Go SSH (`golang.org/x/crypto/ssh`), not shelling out**, and **not sshfs** — sshfs is
  explicitly rejected (FUSE fragility/hangs on drop, needs container privilege that undercuts the
  no-root goal, and only relocates the path-translation problem). If we don't adopt it now, never
  introduce it.
- **Persistent client pool keyed by host** so no per-turn handshake: dial+authenticate **once** per
  host, cache the `*ssh.Client`, open a fresh **session (channel)** per turn (≈free). Keepalive
  goroutine + reconnect-on-failure so a dead link transparently re-dials on the next turn.
- **Slots into the existing seam unchanged:** a new `SSHExecutor` implements `Executor.Start`; the
  returned proc implements `Proc` (`Stdout()` → the channel's stdout, straight into the current
  `parseStream`; `Wait()` → `session.Wait()`). Reuses the exact `claude` argv the code already builds.
- **Cancel** (the fiddly part — SSH signal delivery is unreliable): tag each remote command with a
  unique token and, on ctx-cancel, open a **second cheap channel on the same live client** to kill the
  tagged process group. Handshake-free, and avoids a PTY (which would corrupt the stream-json stdout).
- **`Session.Host`** field (empty = loopback), chosen in the spawn dialog like host/sandbox is today;
  sandbox-over-SSH becomes "SSH to host, then `podman`", still uniform.
- **Discovery over the same SSH channel**, not a mounted FS: a small remote command lists sessions and
  cats only the specific `~/.claude/projects/.../<session_id>.jsonl` we need (we only ever read a
  handful), so no FUSE, no privilege, one transport. Replaces today's local-filesystem discovery.
- **Security:** verify host keys against a known-hosts file (no blind-trust), auth via ssh-agent or a
  configured key; new `SPAWNER_SSH_*` env vars for the key/known-hosts paths.
- **Credential propagation** (copy known-working creds host→host once SSH is up) is a **separate later
  feature** — powerful but widens blast radius, so keep it deliberate and out of the first cut.

**Sequencing:** build the single `SSHExecutor` + pool and prove it against **localhost first** (so the
"real host" is our first remote and we flush out discovery/cancel rework immediately) → then the work
box is nearly free → then containerizing the server is a deploy change, not new code.

**Order of remaining work (user, 2026-07-08):** do **all non-Android (server-only) steps first**,
**Android steps last**. Test Android on the **emulator** throughout; install on the Pixel 8a only once
the feature works as expected, as the ship step (see [[use-android-dev-skill-and-emulator]]).
**Re-containerizing the server is LOW priority** — it blocks nothing, do it whenever.

- [x] 2026-07-08 — **Host-scoped directory browser (sidebar "new session").** The visual picker now
      lists the **chosen host's** filesystem over SSH (loopback for localhost), starting at that host's
      root `/`, instead of the server's local filesystem jailed to `SPAWNER_ROOT` — fixes the bug where
      picking a remote host still showed localhost's files (in a container the server's local FS is just
      a few mounts, so even "localhost" must list over the loopback sshd). `browse` carries `host_name`;
      new `SSHPool.ListDir/DirExists/MakeDir/Run` run the probes remotely; `doSpawnAt` checks/creates the
      dir on the target host and requires an absolute path (spawn-root jail dropped for the visual picker
      — voice dialog still uses the roots). App: host/target moved to the top of the New-session screen;
      changing the host re-lists from its root. Server-only steps verified via `go test`; **needs the
      container redeployed (restart button) to go live, then Android emulator/phone check.**
- [x] 2026-07-08 — **Server-owned SSH auth material.** Private key and known_hosts moved into the
      server's own `deploy/state/` (`/state/ssh/…`, `/state/known_hosts`), independent of the host home.
- [x] 2026-07-08 — **Auto-managed host-key trust.** Adding a host in the app now records its SSH key
      trust-on-first-use (`SSHPool.TrustHost` scans the key in Go, ssh-keyscan style, and appends to
      `/state/known_hosts`); deleting a host forgets its record (`SSHPool.ForgetHost`). The pool
      reloads the file after each change, so trust takes effect **without a restart**. Piggybacks on
      `host_put`/`host_delete` (no new wire messages). Fixes: a newly added host used to fail with
      "knownhosts: key is unknown" and there was no in-app way to trust or remove a key.
- [x] 2026-07-08 — **SSH identities: app-managed keypairs, hosts reference them.** New
      `session.IdentityStore` — the app names/creates keypairs, the server generates ed25519 and
      **keeps the private key** (`SPAWNER_SSH_KEYS` dir, `0600`), exposing only the public key
      (`identity_list`) to copy onto a target host. Wire: `identities` / `identity_create` /
      `identity_delete` → broadcast `identity_list`; `bad_identity` error; `SPAWNER_IDENTITIES` registry
      file. `Host.Identity` names an identity and, when set, supersedes `KeyFile` — the SSH pool
      resolves it to the managed private key. App: a **Settings → Identities** screen (create, list with
      copyable public keys, delete) and a host-form identity picker; the host card shows the linked
      identity. **Import** an existing server-side key (`identity_import` → copies it into the keys dir,
      records its public key) so the config default key that already authenticates turns shows up and
      can be linked. An identity carries a **required username** (a default a host's User overrides)
      and an **optional SSH password** (password auth, key optional — a keyless password-only identity
      is allowed); the password is server-only (never sent; the app sees only `has_password`). Server +
      app + docs + tests, built and verified on the emulator. Needs the container redeployed (restart
      button) + the new APK for the feature to be live end to end.
- [x] 2026-07-08 — **Restart button rebuilds + recreates the container (one-tap deploy).** For the
      container deployment `SPAWNER_RESTART_CMD` now SSHes to the host over loopback and launches
      `deploy/rebuild-container.sh` detached (`setsid`), which runs `compose up -d --build` to rebuild
      the image from source and recreate the container. It must run on the host — `up --build` replaces
      the very container the server lives in — so `setsid` over SSH decouples it to survive the
      teardown. The image now ships `openssh-client` for this. Bare-metal button is unchanged (pure
      `systemctl` bounce). Bootstrap needs one manual `up -d --build` (the running container predates
      the openssh-client image + the env var). Documented in `deploy/README.md` and `CLAUDE.md`.
- [x] 2026-07-08 — **Explicit host model — no implicit localhost default; "Local" is a listed host.**
      `Session.Host` is now always an explicit name (`session.LocalHost = "localhost"` for loopback):
      the `SSHExecutor` errors on a hostless host-target session instead of coercing to localhost, the
      Usage probe and discovered sessions name `localhost` explicitly, and legacy empty-host records
      migrate to `localhost` on store load. The spawn-time default lives in one place (`newSession`),
      so voice/legacy spawns still work while a purely **remote-only deployment** is now possible.
      `localhost` is not a special built-in: `OpenHostStore` seeds it into a fresh registry so it's
      listed out of the box, but it's an ordinary, editable, **deletable** row (delete sticks — the
      file exists after any change, so it never re-seeds). Delete it and the server drives only remote
      machines. App: localhost renders from the registry like any other host in Settings → Hosts and
      the picker (no hardcoded chip); every spawn sends an explicit host. Documented in
      `docs/architecture.md`, including what `localhost` means under the container's host networking
      (`localhost:22` = the host's sshd). Server suite green.
- [x] 2026-07-08 — **`SSHExecutor` + persistent per-host client pool (keepalive + reconnect),
      proven against localhost.** (`internal/session/ssh.go`): pool dials+auths once per host, opens a
      cheap channel per turn, keepalive drops a dead link, executor drops+re-dials once on a stale
      conn. Registered for `TargetHost` when `SPAWNER_SSH=1` (else the direct-fork `HostExecutor`
      stays), so with SSH on, **every** host turn — loopback included — runs over SSH with no
      special-cased local path. **Live-proven over real loopback sshd** (`SPAWNER_SSH_LIVE=1`
      `TestLiveSSHLoopback`: dial → cached-conn reuse → streamed remote output through the quoting
      path). Fixed a Go-vs-OpenSSH host-key gotcha the live test caught: Go doesn't bias host-key
      negotiation toward the algorithm already in known_hosts, so a mismatch now retries once with
      `HostKeyAlgorithms` constrained to the stored key type(s). **Real end-to-end claude turn proven
      over loopback SSH** (`TestLiveSSHRealClaude`: `Driver.Turn` → `SSHExecutor` → pooled conn →
      remote claude → stream-json reply). Remaining before flipping the default + deleting
      `HostExecutor`: verify against a genuinely remote host (the work box), where the local-FS
      discovery/resume assumptions no longer hold (that's the discovery checkbox).
- [x] 2026-07-08 — **Cancel via process-group kill over a second channel (no PTY).** Each turn is
      wrapped `setsid sh -c 'echo <pgid> 1>&2; cd … && exec claude …'`: setsid puts claude in a fresh
      process group whose id rides stderr (out of band from the stream-json stdout, so no PTY is needed
      and stdout stays clean); on ctx-cancel the executor opens a second (handshake-free) channel on the
      same connection and `kill -s KILL -<pgid>`, so claude AND any tool child die together — the remote
      analogue of the host executor's group SIGKILL. **Live-proven** (`TestLiveSSHCancelKillsRemote`: a
      long remote process tree is gone after cancel); real claude turns still pass under the wrapper on
      both loopback and the work box. Unit test pins the wrapper string.
- [x] 2026-07-08 — **`Session.Host` + spawn-dialog host choice; loopback default.** `Session.Host`
      (empty = loopback) is read by the SSHExecutor and routes discovery/resume. `spawn_at` gained an
      optional `host_name`; `doSpawnAt` sets `Session.Host` on the new session (ignored for sandbox).
      The app's New-session browser offers a host picker (Local + configured hosts) that threads the
      choice through. Verified end to end on the emulator: picking a host persists the session with
      that host. (Voice "spawn on <host>" phrasing is a later nicety — the visual picker ships first.)
- [x] 2026-07-08 — **Discovery/resume over the SSH channel.** Built the `claudeFS` seam
      (`internal/session/claudefs.go`) — local (`os.*`) and SSH backends behind the same JSONL parse —
      selected per session by `Driver.claudeFSFor(Session.Host)`, with a host-namespaced transcript
      cache key. Gateway per-session ops now read from the session's host: `serveHistory`
      (`ReadTranscriptChain`), `doAttach` + `startTurn` badge (`LastContextUsage`), and delete
      (`DeleteSessionByIDs`/`DeleteSessionsForDir`) all take `Session.Host`. Live-proven equivalent to
      local over loopback (`TestLiveSSHClaudeFSMatchesLocal`); full suite green (local path unchanged).
      **Deferred:** discovering UNREGISTERED sessions that live only on a remote box (which hosts to
      scan is an open question) — `doDiscover` still scans the local disk, but registered remote
      sessions surface via the store, and their history/attach/usage/delete now work over SSH.
      Two facts the work-box run surfaced that this handles: (1) **`Session.Dir` is a REMOTE
      path** for a remote host (a local temp dir doesn't exist there), so discovery/resume must read
      the remote `~/.claude`, not the server's; (2) **the Go pool dials the literal host string and
      ignores `~/.ssh/config` aliases** — so "work" won't resolve like `ssh work` does; host addressing
      needs the real hostname/IP (or we teach the pool to read ssh_config). Both feed the `Session.Host`
      addressing model.
      **Plan (scoped 2026-07-08):** all on-disk Claude access funnels through a few primitives with
      exactly two `os.UserHomeDir()` sites (`discover.go:24`, `transcript.go:158`) — no existing
      indirection. Introduce a `claudeFS` seam (new file) with primitives — `listTranscripts()`
      (one remote `find ~/.claude/projects -name '*.jsonl' -printf '%T@ %p\n'`), `readWithStat(path)`
      (remote `stat -c '%s %Y' … && cat …`, one round trip → feeds size+modtime cache key AND content),
      `headLines(path,n)` (cwd extraction), `remove(path)`, `findByID(id)`, `globDir(dir)` — with a
      **local branch** (`os.*`, today's code) and an **SSH branch** (pool `NewSession().Output`).
      Keep the JSONL PARSE logic shared (operate on fetched bytes); make the fs-touching funcs go
      through `claudeFS` with package wrappers preserving today's local behavior (existing tests green),
      then thread a host-aware `claudeFS` (from `Driver` + the SSH pool, selected by `Session.Host`)
      into the gateway callers: `doDiscover`, `serveHistory`, `doAttach`/`startTurn` (`LastContextUsage`
      badge), `doDeleteDiscovered`. **Make the transcript cache key host-aware** (prefix with host) so
      identical remote/local paths can't collide. Note: discovery is 1 + N round trips (one `find` +
      one head-read per transcript for cwd); fine over the multiplexed pool, optimize later if needed.
      Staged commits: (a) local `claudeFS` seam, behavior-preserving; (b) SSH branch + host-aware cache
      key; (c) wire gateway callers by `Session.Host`. Do NOT introduce sshfs (epic rule).
- **Host registry (app-authoritative, server-persisted)** — decided 2026-07-08: the app is the
  source of truth for the host list; the server persists it to a JSON file so it survives restarts and
  is shared across clients; **all editing happens in-app**. `Session.Host` names a registry entry; the
  SSH pool resolves the name → address/user/port/key (the Go client dials the literal address — it does
  NOT read `~/.ssh/config`, so entries hold real hostnames/IPs). Server-side first (registry +
  persistence + pool resolution + wire CRUD), the Settings→Hosts page in the Android phase last.
  - [x] 2026-07-08 — **`Host` + `HostStore`** (`internal/session/hosts.go`): name/address/user/port/
        key_file/claude_bin, concurrency-safe, atomic temp+rename persistence (mirrors `Store`).
        `TestHostStoreRoundTrip` covers upsert-in-place, sort, delete, and reload-from-disk.
  - [x] 2026-07-08 — **Pool resolves `Session.Host` via the registry.** `SSHPool` takes the
        `HostStore`; `resolve(name)` maps a host name → address/user/port/key (per-host
        `ClientConfig`; known_hosts callback shared), `binFor(name)` picks the per-host claude binary.
        A name absent from the registry (or a nil store) dials literally with the config defaults, so
        loopback/raw-hostname/tests still work. `SPAWNER_HOSTS` config + `main` opens the store and
        passes it to the pool. `SPAWNER_SSH_*` stays as fallback defaults. **Live-proven**: a logical
        name "workbox" resolved through the registry to the Tailscale IP and drove a real claude turn
        (`TestLiveSSHHostRegistry`); all nil-registry live tests still pass. CLAUDE.md documents
        `SPAWNER_HOSTS` (docsync green).
  - [x] 2026-07-08 — **Wire protocol: `hosts`/`host_put`/`host_delete` + `host_list`.** Gateway
        handlers (`internal/gateway/hosts.go`) list/upsert/delete via `HostStore` and broadcast the
        updated `host_list` to every client so the shared registry stays in sync; `host_put` errors
        `bad_host` on a missing name. `HostStore` threaded through `gateway.New` + `main`. Documented in
        `docs/protocol.md` (3 inbound + 1 outbound + `bad_host` code; docsync green). Wire-level
        `TestHostCRUD` covers list→put→reject-nameless→delete.
  - [x] 2026-07-08 — **[Android] Settings → Hosts page + spawn-dialog host picker.** Settings → Hosts
        lists/adds/edits/deletes hosts over `hosts`/`host_put`/`host_delete`, refreshed from the
        `host_list` broadcast (Protocol `Host`/`HostList`, VoiceController `hosts` StateFlow). The New
        session browser offers a Local + per-host chip picker that sets `Session.Host` via
        `spawn_at host_name`. Built (containerized, per [[spawner-apk-build-signing]]), verified end to
        end on the **emulator** against a scratch server (CRUD persists + broadcasts; spawn sets host),
        then installed on the **Pixel 8a** as the ship step.
- [x] 2026-07-08 — **Drive the work box end to end + re-containerize the server (no root broker).**
      Transport proven (`TestLiveSSHRemoteClaude`: a real authed claude turn on the work box
      `100.64.0.7` over Tailscale, key `bazzite_ed25519`), and the app host picker targets it.
      **Re-containerized:** `server/Dockerfile` (lean static binary — claude runs on the host, not in
      the image) + `deploy/spawner-container.yml` (host networking so `localhost:22` is the host sshd;
      home + roots mounted at the same paths so browse/discovery read where the host writes). Verified
      end to end **in parallel with the live bare-metal binary** (scratch port `:8098`, scratch state):
      a turn dictated through the container ran claude on the host over SSH and streamed the reply
      back — no broker, no host root. This is the clean version of the reverted 2026-07-06
      containerization (SSH replaces the broker). Docs: deploy/README + architecture design note.
- [x] 2026-07-08 — **Host-key verification + ssh-agent/key auth + `SPAWNER_SSH_*` config.** Six env
      vars (`SPAWNER_SSH`, `SPAWNER_SSH_USER`, `SPAWNER_SSH_PORT`, `SPAWNER_SSH_KEY`,
      `SPAWNER_SSH_KNOWN_HOSTS`, `SPAWNER_SSH_CLAUDE_BIN`) in `internal/config`; host keys always
      verified against known_hosts (no insecure mode), auth via ssh-agent and/or a key file; pool built
      + executor registered + closed on shutdown in `main.go`. CLAUDE.md documents the vars (docsync
      green).
- [ ] (Later, separate) credential propagation between hosts.

### Server / infra
- [x] 2026-07-07 — **Fix: the live sandbox test could reap real sessions' containers.**
      `TestLiveSandboxContainer` (`SPAWNER_LIVE=1`) called `ReconcileContainers` with an empty
      known-set, and `SandboxExecutor.List` filters `podman ps` by the shared `spawner-sbx-` prefix
      machine-wide — so the test removed **every** managed sandbox container on the host, including a
      live session's (it destroyed the running `email` session's container mid-work). `SandboxExecutor`
      gained a `Prefix` field (`prefix()` defaults to `containerPrefix`); `List` filters by it, and the
      live tests now run under a unique `spawner-sbxtest-<hex>-` namespace (`NewContainerNameWithPrefix`
      + `liveTestPrefix`) that shares no substring with the production prefix, so a test reconcile can
      only ever see its own containers. `TestSandboxPrefixIsolation` anchors the namespaces don't
      overlap; verified live that a decoy under the real prefix survives the test's reconcile.
- [x] 2026-07-07 — **Sandbox containers bind-mount the server's whole `$HOME` read-write** at the
      same path by default (`SandboxExecutor.HomeMount`, set from `$HOME` in `main.go`), so dotfiles,
      `~/.claude`, and project checkouts are writable in the sandbox exactly as on the host. Built the
      `spawner-sandbox:latest` image from `sandbox/Containerfile` so sandbox turns actually run. Docs
      (README, architecture, sandbox README) updated; `createArgs` test asserts the home mount.
- [x] 2026-07-07 — **Sidebar host-vs-sandbox choice.** The visual new-session screen now shows a
      host/sandbox toggle (host default) like the voice spawn dialog, threading a `target` through
      `VoiceController.spawnAt`/`spawnNewFolder` into `Outbound.spawnAt` (sent as `target` on
      `spawn_at`, already in the protocol spec). Picking sandbox on a server without a sandbox image
      gets a clean `bad_path` error. APK rebuilt.
- [x] 2026-07-06 — **Reverted the containerized-server + broker split; server runs bare metal.** The
      host-side broker existed only so an unprivileged, containerized server could execute on the host,
      but the broker itself ran bare metal and the server never needed root — so the container bought
      almost no host protection while adding a Unix-socket IPC hop and a wire protocol to maintain.
      Folded it back into one binary: the server forks `claude` for host turns and drives the rootless
      runtime for sandbox turns directly (the in-process path that already existed). Deleted
      `cmd/broker`, `broker_proto.go`, `broker_server.go`, `BrokerExecutor`, and the
      `Restarter`/`SessionDeleter`/`DirMaker` delegation interfaces. Restart now fires
      `SPAWNER_RESTART_CMD` (replacing `SPAWNER_BROKER_SOCKET` + the two broker restart vars): a
      detached command in its own process group that rebuilds + relaunches the server, surviving its
      own teardown via the systemd unit's `KillMode=process`. Deploy is now a bare-metal systemd user
      service (`deploy/spawner-server.*`, rewritten `rebuild.sh`); the server Dockerfiles and the
      broker/dev compose stacks are gone, `docker-compose.yml` is whisper-only. Tests: the live
      broker/sandbox-via-broker tests became direct host/sandbox tests; the restart tests assert the
      command fires (or `restart_failed` when unset). Docs (README, architecture with a "don't
      re-introduce" design note, protocol, CLAUDE.md, deploy/sandbox READMEs) updated; docsync green.
- [x] 2026-07-05 — **Fix the bouncing 🧠 context-size counter.** The live counter used the stream
      `result` event's usage, which is the turn's AGGREGATE — it sums every internal tool-step of an
      agentic turn (each step re-reads the whole context), so a tool-heavy turn reported millions of
      "context" tokens vs a real ~430k, and it jumped around with tool-use count. It also disagreed
      with the on-attach value (which correctly reads the transcript's last assistant message). Fixed:
      the post-turn `output` badge now derives context size from `LastContextUsage` (last message),
      the same source as attach, so live and on-attach agree. `turnUsage` still feeds the cumulative
      spend estimate, where summing across steps is correct.
- [x] 2026-07-06 — **Auth/transport hardening: optional server TLS + mutual TLS.** Layered on top of
      the shared token and fully backward compatible (empty = plain `ws://`, still fine behind
      Tailscale). New env vars `SPAWNER_TLS_CERT`/`SPAWNER_TLS_KEY` (both or neither → serve `wss://`)
      and `SPAWNER_TLS_CLIENT_CA` (PEM CA bundle → `tls.RequireAndVerifyClientCert`, so a client must
      present a cert signed by that CA *in addition to* the token; requires the server pair). Config
      validates the cross-constraints at startup; `Config.BuildTLSConfig()` builds the pool; `main.go`
      switches to `ListenAndServeTLS` and logs the scheme (`ws`/`wss`/`wss+mTLS`). Tests:
      `TestLoadTLSValidation` (all cert/key/CA combos) + `TestBuildTLSConfig` (disabled/bad-CA/real-CA
      → mTLS). docsync green (three vars documented in CLAUDE.md); README security section documents
      setup. mTLS is reachable today by CLI clients; the Android client-cert half is the follow-up
      below.
- [x] 2026-07-05 — **Attached-session title tracks the session by stable id, not name.** The app
      keyed the attached session by name; the temporary Dev/Prod toggle gives the same on-disk
      session different names on each server (e.g. `spawner-2` vs `spawner-3`), so switching servers
      left the title showing a stale name and a sidebar rename couldn't line up (name compare missed).
      The `attached` and `renamed` wire messages now carry `session_id`; the app tracks `_attachedId`,
      matches renames by id, and re-derives the title from every fresh session list by id — so the
      title always reflects the current server's name for the attached session. (protocol.md updated.)
- [x] 2026-07-05 — **Restart button can also restart the broker.** New optional
      `SPAWNER_BROKER_RESTART_SELF_CMD` (e.g. `systemctl --user restart --no-block spawner-broker`):
      after launching the server rebuild, the broker runs it to restart itself, so a new broker
      binary / edited `broker.env` is picked up too. Needs `KillMode=process` on the broker unit
      (added to `deploy/spawner-broker.service`) so the detached server rebuild survives the broker's
      own teardown. Also documented that the RestartCmd's compose needs `SPAWNER_TOKEN` in the
      broker env (its absence is why the restart button was silently failing with exit status 1).
- [x] 2026-07-05 — **Fix interrupted-turn session bricking.** `Driver.Turn` flipped `Started`
      false→true only after a clean `Wait`, but claude creates the session on disk the moment it
      launches. A turn interrupted mid-stream (client drop, container restart) left `Started=false`
      with the id already on disk, so every later turn re-ran `--session-id <existing-id>` →
      `claude exited: status 1` forever (seen live on `claude_spawner`/`claude_spawner-2`; this is
      the "sessions deleted/rotated / failed" symptom — it's the compaction rotation path plus an
      interruption). Now `Turn` flips `Started` on launch and `gateway/jobs.go` persists it (and
      drops the consumed `PendingSeed`) on the error path, so the next turn resumes cleanly.
- [x] 2026-07-05 — **Restart button rebuilds + relaunches the containerized server.** The old path
      (exit non-zero, let a host systemd `ExecStartPre` `go build` relaunch) no longer rebuilds now
      that the server always runs as a Docker container. `restart` now routes through the broker: a
      new `opRestart` + `BrokerServer.RestartCmd` (`SPAWNER_BROKER_RESTART_CMD`, a `docker compose …
      up -d --build`) launched detached on the host; `Restarter` interface + `Driver.Restart`;
      `doUsage`-style failure report (`restart_failed`) when there's no broker/command. Retired the
      dead `RequestRestart`/`RestartRequested` + `main()` exit-for-relaunch. Tests: gateway (fake
      Restarter triggers rebuild + no-broker fails) and broker (unconfigured refuses, configured
      runs the command).
- [x] 2026-07-05 — **Docs are Docker-only.** Removed the retired host-native/`go run`/systemd
      deployment from all docs (README "Try it on the host" section + `deploy/spawner.service` +
      `deploy/spawner.env.example` deleted; `deploy/README.md` rewritten for the broker; CLAUDE.md,
      protocol.md, architecture.md, whisper/README.md, compose comments updated). The containerized
      server + host broker is now the only documented deployment.
- [x] 2026-07-05 — **`/usage` runs in a jail-allowed root.** `Driver.Usage` no longer hard-codes
      `/tmp` (rejected by the broker jail); `Driver.UsageDir` is set to the first spawn root.
- [ ] Vocab-bias tuning: measure whether the `--prompt` session-name biasing actually improves
      recognition of real session names/paths, adjust if not. *(biasing itself is implemented)*
- [x] 2026-07-05 — **Containerized server + per-session execution target (host vs sandbox).**
      `session.Driver.Turn()`'s launch is now pluggable via an `Executor` interface
      (`internal/session/executor.go`); durable `Session.Target` (`host`/`sandbox`, default host)
      chosen at spawn time (voice `await_target` step + `spawn_at` `target` field, shown only when a
      sandbox image is configured). Three executors: `HostExecutor` (direct exec), `SandboxExecutor`
      (rootless container, `SPAWNER_SANDBOX_*`), and `BrokerExecutor` → host-side broker daemon
      (`cmd/broker` + `internal/session/broker_*.go`). The broker is the **single host-side agent for
      both targets**: a containerized, unprivileged server routes ALL turns through it
      (`SPAWNER_BROKER_SOCKET`) — it forks `claude` for host turns and drives rootless Podman for
      sandbox turns (ensure/exec/remove/list ops), reusing the same executor code, so the server needs
      neither host root nor a runtime socket. The broker enforces the `SPAWNER_ROOT` jail and owns the
      sandbox runtime config. No component holds host root. Design in `docs/architecture.md`; tests
      cover selection, sandbox argv, broker round-trip/jail, and the spawn target step.
      Sandbox containers are **persistent per session**: created at spawn (`ensureSandbox`), reused
      by every turn via `exec`, removed on delete (`removeSandbox`); `Ensure` is idempotent and
      re-run before each turn so a container lost to a restart is recreated.
      Orphaned sandbox containers (session deleted while the server was down) are swept at startup
      by `Driver.ReconcileContainers` (matched by the `spawner-` name prefix).
      Each session's target rides the `discovered`/`session_list` feed (`target`, sandbox-only) and
      the app badges sandbox sessions ("📦 sandbox") in the sidebar; APK built, installed on the
      emulator + Pixel 8a. **Live-verified on the host** (`SPAWNER_LIVE=1 go test ./internal/session
      -run TestLive`, skipped otherwise): the broker forks the real host `claude` and streams a real
      turn back; the persistent sandbox lifecycle (create → reuse across turns → list →
      reconcile/remove) runs on **rootless Podman**; and a **real Claude turn runs inside the Arch
      sandbox** (`sandbox/`, host claude + auth bind-mounted, `--userns=keep-id`); and a **real Claude
      sandbox turn driven THROUGH the broker** (ensure → turn → reconcile over the socket); and the
      **fully containerized server** — lean broker-mode image (`server/Dockerfile.broker`: binary +
      ffmpeg only), `docker-compose.broker.yml`, broker as a systemd user service
      (`deploy/spawner-broker.*`) — verified end to end (unprivileged server container → broker →
      real claude for BOTH a host and a sandbox turn). **Now the live deployment:** the app runs
      against the Docker server container (uses `claude_spawner` sessions through it), the broker is
      a lingering systemd user service, both auto-start on boot, and the boot order is decoupled via
      a persistent broker-socket directory mount. Remaining manual step (needs root): stop + disable
      the old native `spawner` systemd system service — `sudo systemctl disable --now spawner`.

### Android
- (nothing open — hands-free verified; voice rename shipped, see _Done_)

### Later / nice-to-have
- [ ] Plumb the wake-token alias list (`command.wakePhrases`) through the same pipeline as command
      aliases (→ `docs/commands.json` → `generateCommands` → app), so wake mishearings are visible
      and **editable in the app's alias editor** like regular commands. Server list is authoritative
      today; this makes it user-tunable on-device.
- [ ] **Slim the Kokoro TTS runtime — drop the PyTorch VRAM tax.** The `kokoro` GPU service
      (`ghcr.io/remsky/kokoro-fastapi-gpu`) is Python/PyTorch and pays a fixed ~1.5–2 GB VRAM
      framework overhead for an 82M-param model (measured 2026-07-14: ~2.4 GB resident, vs
      whisper.cpp's lean ~3.8 GB which is nearly all real large-v3 weights). Kokoro ships official
      ONNX weights, so swap the service for a compiled ONNX runtime — [sherpa-onnx](https://github.com/k2-fsa/sherpa-onnx)
      (C++, Go/Rust/Python bindings, no PyTorch) or the Rust [Kokoros](https://github.com/lucasjinreal/Kokoros).
      **Keep it on the GPU** (fast, and beats the host CPU) — the goal is only to cut the base
      overhead to a few hundred MB (CUDA context), not move it off-device. Frees VRAM for whisper +
      the LiveKit wake-word model. Same lesson as whisper.cpp vs PyTorch. Later item; tackle after
      the detector epic lands.
- [ ] On-device fallback STT when offline.
- [ ] iOS app.

## Hardening backlog (2026-07-05 fragility audit)

Ranked, verified findings from a full-codebase audit. The store's `flush` and usage `persist`
are already atomic (temp+rename), so several "corruption" claims were discounted; the per-conn
read loop is single-goroutine, so several "attach/detach race" claims were discounted too. What
remains real:

_Done in this pass:_
- [x] 2026-07-05 — Surface `SPAWNER_WHISPER_FAST_MAX_SEC` parse errors at startup instead of
      silently falling back to 2.5s (`config.go`).
- [x] 2026-07-05 — Bound `OggOpusToPCM` ffmpeg decode with a 30s context timeout so a hung
      ffmpeg can't pin a goroutine forever (`transcribe.go`).
- [x] 2026-07-05 — Log (instead of swallow) corrupt-state reads and persist failures in the
      usage estimator (`usage.go`).

_Extensibility (the "easier to extend, not delicate" asks):_
- [x] 2026-07-05 — Server wire dispatch is now a single registration table (`wireHandlers`
      `map[string]func(*conn, inbound)` in `gateway.go`); `loop()` just looks up + calls. Adding a
      message means one map entry (+ a docs/protocol.md line — `docsync` now parses the map keys and
      still fails the build on an undocumented type). The voice-command path was already single-
      sourced through `runCommand` (shared by `dispatch` and the hands-free commit in `stream.go`).
- [x] 2026-07-05 — Android dispatch `when` confirmed compile-time exhaustive: on Kotlin 2.0 a
      statement `when` over the `ServerMsg` sealed interface with no `else` errors if a variant is
      unhandled, so a new server message can't be a silent no-op. Documented the intent (and the
      "don't add an `else`" rule) at the `when` so the guard isn't accidentally removed.
- [x] 2026-07-05 — Rename now migrates ALL name-keyed client state via one `migrateSessionKey(old,
      new)` helper (`logs`, `oldestIndex`, `hasMore`, `loadingOlder`) — previously only logs/hasMore,
      so a rename mid-page-load stranded the `oldestIndex` cursor / `loadingOlder` flag. The helper
      is the single site that knows the full set, so a future keyed map gets migrated in one place.
- [~] Centralize turn-completion on the client — SKIPPED. `_lastTurnUsage`/`_attachedName` are
      written at genuinely distinct transitions (attach/detach/rename/output-done/context-reset)
      with per-site variations, not repeated duplication; a flag-taking `completeTurn()` helper would
      reduce clarity, not fragility. The bug this targeted (rename orphaning) is now solved
      structurally by `migrateSessionKey`. Revisit only if a concrete drift bug reappears.

_Robustness / ops (smaller, safe when we get to them):_
- [x] 2026-07-05 — `parseStream` now counts non-blank unparseable lines and, when the stream ends
      with no result event, reports "stream corrupted: ... (N malformed lines)" so a truncated
      claude stdout is diagnosable (`session.go`; `TestParseStreamReportsCorruption`).
- [x] 2026-07-05 — Transcript parses are memoized per file, keyed by size+modtime, so attach
      (`LastContextUsage`) and history paging (`ReadTranscriptChain`) stop re-reading whole
      ever-growing transcripts. Append-only files self-invalidate on the next stat — no explicit
      invalidation needed (`transcript.go`; `TestTranscriptCacheInvalidatesOnChange`).
- [x] 2026-07-10 — Validate the audio `codec` field: unknown values are rejected with `bad_message`
      before any capture starts, instead of silently treated as PCM16 (`audio.go`; shared
      `codecPCM16`/`codecOggOpus` constants mirrored by the client's `Codecs` object;
      `TestAudioUnknownCodecRejected`).
- [x] 2026-07-05 — Loud startup warning when `SPAWNER_ROOT` is empty (unrestricted spawn scope)
      (`main.go`).
- [ ] Graceful shutdown waits briefly for an in-flight turn instead of a hard 5s HTTP-server kill.

_2026-07-10 hardening pass (drift-proofing + error handling):_
- [x] 2026-07-10 — **Client↔server wire drift tests** (`docsync/clientsync_test.go`): the Kotlin
      client's single wire file (`net/Protocol.kt`) is cross-checked against the Go gateway both
      ways — every type the client sends must have a `wireHandlers` entry, every type the server
      emits must have a `ServerMsg.parse` branch (and vice versa), and the audio codec constants
      must agree on both sides + be documented. Deliberately one-sided messages (e.g. `reply`,
      `session_list`, `ping`/`pong`) are recorded in exemption maps with reasons, so "the app
      doesn't use this" is a decision, not drift.
- [x] 2026-07-10 — The error-code docsync test now also scans `c.fail(...)` call sites (it only
      caught `msgError(...)` before); that immediately surfaced the undocumented `bad_agent` code,
      now in protocol.md's error table along with `restart_failed`.
- [x] 2026-07-10 — Stop silently ignoring persistence/IO errors: session-store `Put`/`ForgetID`/
      `Delete` failures in the turn/rotation paths are logged (`jobs.go`, `ops.go`), and the
      whisper HTTP body-read errors propagate instead of being read as garbage (`remote.go`).
- [x] 2026-07-10 — Prefs defaults single-sourced: every non-zero settings default lives once in
      the `Prefs` companion (`commonMain`), referenced by both backends (`SettingsStore`,
      `WebPrefs`) — the two stores can no longer disagree on a default.
- [x] 2026-07-10 — `docs/web-client.md`: developer guide for the wasmJs client (source-set split,
      the `js()` interop idiom and its conventions, compile gate, iterate loop) + a doc-map row in
      CLAUDE.md.
- [x] 2026-07-10 — docsync now verifies protocol.md **payload field names** both ways
      (`fieldsync_test.go`): every json tag / `msg*` map key must be documented, and every field the
      protocol tables name must exist in the code. Caught two real drifts on landing: `hello`'s
      documented `app_version` (never read; the real field is `client_id`) and `discovered`'s
      undocumented `host` field.
- [x] 2026-07-10 — Commands pipeline end-to-end check: `CommandsSyncTest`
      (`:app:testDebugUnitTest`, the app's first JVM unit test) compares the compiled-in `COMMANDS`
      list against `docs/commands.json` entry by entry, so a `generateCommands` translation bug
      (dropped command, escaping, sorting) fails the build. Verified it fails on an injected
      generator bug.

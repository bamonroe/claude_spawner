# Client refactor — what's left to do

Task tracker for the Kotlin Multiplatform client refactor (branch `feat/web-client-compose-mpp`).
A background agent (Fable) worked this twice but was killed by session limits both times **before
any commit landed**, so the working tree is clean and all of its edits are lost. This file
reconstructs its plan from its transcript so the work can be redone cleanly.

Everything below is **behavior-preserving** unless noted. Commit each coherent unit the moment it
builds/tests clean (do not push). Verify on the **emulator only** — never the Pixel 8a here. Do
**not** touch `net/Protocol.kt` wire strings (the Go `internal/docsync` tests cross-check them).

Build gate (both required before committing a code change):
- Android APK: **clean** containerized Gradle build (KMP incremental ships stale dex → launch
  crash). See the `android-dev` skill at `/data/android`; JDK 21 at `/home/bam/opt/jdk-21.0.11+10`.
- Web: `:app:wasmJsBrowserDistribution` compiles.
- `androidUnitTest` passes.

---

## Task 1 — Extract shared ServerMsg handling into commonMain  *(NOT STARTED — the big one)*

`androidMain/.../VoiceController.kt` (~1487 lines) and `wasmJsMain/.../WebAppController.kt` (~618
lines) handle the server's `ServerMsg` stream with ~80% duplicated logic. VoiceController also keeps
~8 parallel per-session maps keyed by session key, with a `migrateSessionKey()` smell when the key
changes.

To do:
- Add a commonMain per-session **`SessionState`** data class + a store keyed by session key,
  collapsing the ~8 parallel maps into one map of `SessionState` (kills `migrateSessionKey()`).
- Add a commonMain **shared reducer** that applies `ServerMsg` events to the store, with
  **platform hooks** (interfaces/callbacks) for platform-specific effects — TTS/speech on Android,
  DOM/audio on wasm.
- Move only the clearly-duplicated handling; leave genuinely platform-specific handling behind the
  hooks. Prefer safe, verifiable increments over a total rewrite.

Context Fable had already read for this: `Prefs`, `HostsIdentitiesController`, `ChatModels`,
Markdown/Clock helpers, and the two controllers. It had **not** written any of the store/reducer
yet.

## Task 2 — Remove hardcoded "claude" UI defaults  *(DONE by Fable but LOST — redo)*

Fable's approach (recreate it):
- **New file** `commonMain/.../Agents.kt` with:
  - `const val FALLBACK_AGENT_ID = "claude"` — fallback only until the server's `agents` registry
    arrives (or on a pre-agent server).
  - `fun defaultAgentId(agents: List<AgentInfo>): String = agents.firstOrNull()?.id ?: FALLBACK_AGENT_ID`
    — the server lists its default backend first, so this resolves "the default agent".
- `MainScreen.kt` (two sites): `d.agent.ifBlank { "claude" }` → `d.agent.ifBlank { defaultAgentId(agents) }`.
- `Sidebar.kt` `backendBadge(...)`: drop the backend prefix for `defaultAgentId(agents)` instead of
  the literal `"claude"`; fall back to a capitalized id on a pre-agent server.
- `SettingsScreens.kt` "Remote claude binary" field: **leave the label as-is** — Fable determined
  this is NOT a default-backend placeholder. The host's `claude_bin` overrides only the Claude
  backend's binary on that host (server `SSHPool.binFor`); other backends use their own config
  (e.g. `SPAWNER_SSH_CODEX_BIN`). Just add a comment saying so; no functional change.

## Task 3 — Client lifecycle fixes  *(one DONE-but-lost, two NOT VERIFIED)*

a. **lostTurnWatchdog false-positive race** (VoiceController) — *NOT investigated yet.* Suspected
   the watchdog can fire for a turn that already completed or a different turn. Verify it's real,
   then tie the watchdog to a **turn identity** so a stale timer can't clobber a new turn. If it
   turns out to be a non-bug, document why instead of changing it.

b. **LevelMeter never stopped on background** (MainActivity) — *DONE by Fable but LOST.* The
   standalone mic meter (Audio settings screen) held the mic open in the background. Fix Fable
   applied: replace the `micMeter` `DisposableEffect(Unit) { startMeter(); onDispose { stopMeter() } }`
   with `LifecycleStartEffect(Unit) { startMeter(); onStopOrDispose { stopMeter() } }` so it
   releases the mic on `ON_STOP` and restarts on `ON_START`. Requires importing
   `androidx.lifecycle.compose.LifecycleStartEffect` and removing the now-unused
   `androidx.compose.runtime.DisposableEffect` import.

c. **VoiceController scope vs Activity lifetime** — *NOT investigated yet.* Inspect whether the
   controller is scoped to something longer-lived than the Activity (may be intentional for
   background turns). If intentional, add a short comment documenting the ownership model instead
   of changing it.

## Task 4 — Build, test, verify  *(NOT STARTED)*

- Clean containerized APK build + `:app:wasmJsBrowserDistribution` + `androidUnitTest`.
- Smoke-test on the **emulator only**.
- Commit atomically per unit; do not push.

---

### Suggested commit order (small units survive a cutoff)
1. Task 2 — `Agents.kt` + the three UI-file edits (compiles fast, low risk).
2. Task 3b — MainActivity LevelMeter lifecycle fix.
3. Task 3a / 3c — after verifying they're real.
4. Task 1 — the commonMain store + reducer, ideally split into: (a) `SessionState` + store, then
   (b) reducer + wire each controller onto it one at a time.

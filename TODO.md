# TODO — claude_spawner

The **live task list** for active and recently-completed work. This is the single source of
truth for what's in flight; `README.md` keeps the historical phase-by-phase roadmap.

**Maintenance rule** (see `CLAUDE.md`): edit this file in the same commit that proposes or
completes a feature **or a test**. Adding a feature/test → add an unchecked box here. Finishing
one → check it off (move to _Done_, dated). Dropping a test/feature → remove it with a one-line
why. A change that leaves this file stale is incomplete.

Dates are `YYYY-MM-DD`.

## Active

### Web client via Compose Multiplatform — no-divergence with the app (proposed 2026-07-08)

**Why:** ship a browser client that mirrors the Android app exactly, with zero UI drift. Since the
app is 100% Jetpack Compose, we convert it to **Compose Multiplatform**: one `commonMain` renders the
same composables on Android **and** in the browser via **Kotlin/Wasm**. Mobile web view == the app;
desktop web view adds the sidebar (Compose `WindowSizeClass`, sidebar when wide). Hosts/identities are
server-side already, so both clients see the same state once the web client hits the same server.

Milestones:
- [x] 2026-07-08 — **M1 — KMP scaffolding.** `app` is now Kotlin Multiplatform + Compose Multiplatform
      with `commonMain` / `androidMain` / `wasmJsMain`. Existing app code moved verbatim into
      `androidMain`; a shared `App()` (with `expect/actual platformName()`) renders on both. Verified:
      `:app:assembleDebug` produces the APK **and** `:app:wasmJsBrowserDistribution` produces the web
      bundle (index.html + spawnerweb.js + .wasm). `generateCommands` now feeds `commonMain`. Repo hygiene:
      dropped `FAIL_ON_PROJECT_REPOS` (the Wasm toolchain injects its own binaryen/node download repos).
      Env note: the box's pinned JDK 21 had vanished (breaking all Gradle builds) — restored to
      `/home/bam/opt/jdk-21.0.11+10`.
- [x] 2026-07-08 — **M2 — Multiplatform networking.** `net/Protocol.kt` moved to `commonMain`, ported
      from Android `org.json` to multiplatform `kotlinx-serialization-json` (JsonElement API; same public
      `ServerMsg.parse` + `Outbound` builders, data classes untouched). `net/SpawnerClient.kt` is now a
      shared Ktor client (reconnect/backoff, hello handshake, ordered outbox channel); `ClientTls` + the
      HTTP-client factory are `expect`/`actual` — Android keeps the OkHttp engine + mutual-TLS client cert,
      web uses the browser WebSocket (Ktor Js engine). Both targets compile + build (`:app:assembleDebug`
      and `:app:wasmJsBrowserDistribution`). Deferred: a **live** browser connect+hello test — needs the
      server/token connect UI, which is still in `androidMain` MainActivity; wire it once M3 shares that UI.
- [ ] **M3 — Shared UI.** Move the pure-Compose screens (chat, sidebar, hosts/identities/server/audio/
      appearance/commands settings, browse) into `commonMain`; abstract platform pieces (mic, wake word,
      TTS, permissions, SAF file pickers, prefs) behind `expect`/`actual`. Web stubs where no browser
      equivalent yet.
  - [x] 2026-07-08 — **Hosts + Identities screens** (first shared slice). `SettingsScaffold`,
        `HostsSettings`, `IdentitiesSettings` lifted verbatim into `commonMain/SettingsScreens.kt`,
        retyped against a new shared `HostsIdentitiesController` interface (VoiceController implements it);
        `collectAsStateWithLifecycle` → common `collectAsState`. Both targets build. These were the natural
        first pick — their `Host`/`Identity` types + `Outbound` builders were already shared in M2, and the
        server owns the registries so both clients edit the same data.
  - [x] 2026-07-08 — **Chat message rendering** (`ChatList`, `Bubble`, `TokenBadge`) + `MarkdownText`
        lifted into `commonMain`; `Role`/`ChatMessage` moved to a shared `ChatModels.kt`; `fmtTok`
        rewritten without JVM `String.format`; `fmtStamp` is now `expect`/`actual` (Android
        `SimpleDateFormat`, web `Intl`/JS `Date` via `js()`). Both targets build. The `MainScreen`
        orchestrator (permissions, pickers, audio) stays in `androidMain` and calls the shared pieces.
  - [x] 2026-07-08 — **Chat status chrome** (`DetachedBanner`, `SpeakingBar`, `ActivityIndicator`,
        `AskDialog`, `DraftLine`, `VoiceStatePill`) lifted into `commonMain/ChatStatus.kt`; `VoiceState`
        enum moved to shared `ChatModels.kt`. All pure Compose — no new seams. Both targets build.
  **NEXT STEPS (M3 remaining) — read before resuming.** What's shared so far: the whole net layer
  (M2), the Hosts/Identities screens, and the chat *presentation* (`ChatModels`, `ChatRendering`,
  `ChatStatus`, `MarkdownText`, `SettingsScreens`, plus `App.kt`/`platformName`). What's still
  Android-only is the **orchestration** — `MainActivity.kt` (~2000 lines now) holds `AppRoot`,
  `MainScreen`, `InputBar`, `TopBar`, the `Sidebar`, the remaining settings screens, `BrowseScreen`,
  and all the platform plumbing (permissions, SAF pickers, audio, notifications, `SettingsStore`).
  The two structural enablers below unblock everything else; do them first, in order.

  - [ ] **(a) Widen the shared controller interface.** Today `HostsIdentitiesController`
        (`SettingsScreens.kt`) only covers hosts/identities. `MainScreen`/`Sidebar`/`InputBar` read a
        lot more `VoiceController` state (`chat`, `status`, `connected`, `attachedName`, `activity`,
        `pending`, `voiceState`, `speaking`, `usage`/`rateLimit`/`usageEstimate`, `discovered`,
        `hasMoreHistory`, `scrollTick`, …) and call many methods (`sendText`, `attach`/`detach`,
        `wake`/`commit`, `abort`, `loadOlder`, `discover`/`adopt`/`rename`/`delete`, `spawnAt`, …).
        Define a broad `AppController` interface in `commonMain` exposing these as `StateFlow`s +
        methods; make `VoiceController` implement it (mark members `override`). Keep audio/mic/TTS OUT
        of it — those stay Android-only and are driven by the concrete class, not the shared UI.
  - [ ] **(b) Prefs abstraction.** `SettingsStore` is `SharedPreferences` + `context.filesDir` (client
        cert). Extract a `commonMain` interface (e.g. `Prefs`) with typed get/set for the keys the
        shared settings screens need; Android `actual` wraps SharedPreferences, web `actual` wraps
        `localStorage`. Leave the client-cert file I/O in the Android impl only.
  - [ ] **(c) Then lift, screen by screen (each its own commit, both targets green):**
        - Settings that are mostly prefs + server sends: `ServerSettings`, `AppearanceSettings`
          (needs `ThemeMode` shared), `CommandsSettings` (uses shared `COMMANDS` already),
          `AudioSettings` (mic-meter/calibration bits stay Android — split them out or stub on web).
        - `SettingsHub` + `SettingsRow` (trivial, pure) — move alongside.
        - `TopBar` + `AudioOutputButton`: share the `AudioOutput` type first (a small data class:
          icon/label/id), keep the actual audio-routing (AudioRouter) Android-only behind the
          controller. `CacheWarmBar` needs a monotonic-clock seam (`expect fun nowMonotonicMs()` or
          switch `TurnUsageInfo` to `kotlin.time.TimeSource.Monotonic`).
        - `Sidebar` (sessions list, usage footer, `UsageEstimateLine`, `SessionLimitFooter`,
          `UsageSheet`, `UsageBar`) — mostly pure over shared types; move the small `fmtTokL`/`pctStr`
          helpers to common too (rewrite any `String.format`).
        - `InputBar` — the text field + send is pure; the 📎 transfer + mic button are platform
          (SAF pickers / audio) → gate behind controller callbacks + `expect` pickers, web-stub them.
        - `MainScreen` + `AppRoot` shell last, once its children are shared; the Activity keeps
          permissions/lifecycle/service wiring and just hosts the shared `AppRoot`.
  - [ ] **(d) Remaining platform seams to add as needed:** clipboard is ALREADY common (used in the
        Identities screen). Still need: prefs (b), monotonic clock (CacheWarmBar), file pickers
        (InputBar 📎), status-bar chrome (`ui/Theme.kt` `setStatusBarAppearance` → `expect/actual`;
        web no-op). Audio capture / wake word / TTS / notifications / foreground service are M5 (web
        gets Web Audio + `SpeechSynthesis`); until then the web controller stubs them.
  - [ ] **(e) Then M2's deferred check becomes doable:** once `ServerSettings` (server URL + token)
        is shared and a minimal web `AppController`/`main.kt` wires a real `SpawnerClient`, verify a
        live browser connect + hello handshake against the running server.
- [ ] **M4 — Responsive layout.** `WindowSizeClass`: phone/narrow == app drawer; desktop/wide == persistent
      sidebar. Same composables, different container.
- [ ] **M5 — Web-native platform bits.** Browser audio (Web Audio → server STT), `SpeechSynthesis` TTS,
      browser file up/download for the 📎 flow, `localStorage`-backed prefs.
- [ ] **M6 — Serve + document.** Static web bundle served (behind the authenticated WS + TLS); README
      web-client section; `docs/` updated. No divergence check: both targets build from one UI source.

### File upload/download over the WebSocket (proposed 2026-07-08)

A 📎 button left of the message box transfers files between the phone and the session's host, over the
same authenticated socket (base64 in one message each way, 64 MiB cap).

- [x] 2026-07-08 — **Server half.** New `upload` (write a base64 file to `<dir>/<name>` on `host_name`)
      and `download` (read a file, return `file_data` base64) messages; `browse` gained a `files` flag so
      the picker can also list regular files (`listing` entries now carry a `dir` flag, directories first).
      `SSHPool.ReadFile/WriteFile/ListAll` do the host-side I/O over the pooled SSH connection (loopback
      for local); local-FS fallback when SSH is disabled. Docs: `docs/protocol.md` (`upload`, `download`,
      `file_saved`, `file_data`, `file_too_large`), README. Errors: `file_too_large`, `bad_path`.
- [ ] **Android half.** 📎 button left of the input bar → upload/download menu. Upload: SAF `OpenDocument`
      to pick a local file, then the host-scoped browser (starting at the attached session's dir) to pick a
      destination folder → send `upload`; on `file_saved`, prefill the draft with `look at the file at <path>`
      (do **not** send). Download: files-mode browser to pick a file → `download`; on `file_data`, SAF
      `CreateDocument` to save it. Then install on the Pixel 8a.

### De-fragilize session identity (epic — make `session_id` the identity, not the name)

**Why:** today a *directory* is treated as the session and the mutable *name* is the primary key
everywhere — the store (`byName`), the turn hub (`jobs` by name), the in-flight tracker, and every
wire command resolve by name; discovery collapses to one row per directory; delete wipes a whole
directory. So multiple `session_id`s in one dir get hidden, renames land on whichever record wins the
`byDir` map, the Dev/Prod split gives the same session different names, and a rename orphans
name-keyed client state. Root fix: the stable `session_id` is the identity; the name is a display
label. (Full code map established 2026-07-05 via two Explore passes — server + Android.)

- [x] 2026-07-06 — **Unregistered-dir delete now wipes the whole directory again.** After Phase 1 made
      delete per-`session_id`, an *unregistered* row (which discover still collapses to one row per dir)
      only removed one of the dir's loose transcripts, so the row reappeared on a dir-mate and looked
      undeletable (e.g. the `/data` "data" row with two transcripts). `doDeleteDiscovered` now splits:
      registered rows delete by ids (unchanged); unregistered rows use `DeleteSessionsForDir`.
- [x] 2026-07-05 — **Phase 1 — server discovery/rename/delete became per-`session_id`.** `doDiscover`
      emits every registered session as its own row (keyed by its own `session_id`), not one collapsed
      row per dir; `doRenameDiscovered` resolves the target by `session_id` (not `GetByDir`); delete
      targets a single session's transcript(s) via `DeleteSessionsByIDs` + a per-id broker path
      (`brokerRequest.IDs`) instead of nuking the whole directory. Fixes hidden sessions + renames and
      deletes hitting the wrong one. Tests: discover-shows-every-session, per-session delete (gateway +
      broker). protocol.md updated (docsync green).
- [x] 2026-07-05 — **Phase 2 — server keys turn state by `session_id`.** Store gained a `byID` index
      (O(1) `GetBySessionID`; a rename only re-keys `byName`). The `jobs` hub, `inflight` tracker, and
      `interrupted` map key by `session_id`; `renameJob` deleted (a rename no longer re-keys anything).
      Because a compact/clear ROTATES the `session_id`, the two rotation sites now `rekeyJob` the hub
      and `ForgetID` the old index entry so turns still reach attached devices. Tests: rename-then-turn
      still delivers; compaction fan-out; per-session delete. (Wire `attach`/`history` still by name —
      resolved to the record server-side; app-side id keying is Phase 3.)
- [x] 2026-07-05 — **Phase 3 — attach by stable id across servers.** Wire `attach` now accepts a
      `session_id` (server resolves it to the current name; `doAttachBy`), so the app re-attaches to
      the SAME session even when it's named differently on the other server. App persists
      `lastSessionId`, auto-attaches by it on reconnect, and highlights the attached sidebar row by id
      (`attachedId` StateFlow) instead of name. Tapping a session already adopts by id. protocol.md
      updated (docsync green); APK builds clean. (Chat-log map is still name-keyed but self-corrects
      via the history refetch on every attach — deferred as a nicety.)
- [x] 2026-07-05 — **Phase 4 — stop minting same-folder duplicates.** Opening a directory that
      already has a registered session now attaches to it instead of minting a `-2` — both the app
      browser (`doSpawnAt`) and the voice spawn dialog (`beginAttachQuestion`) reuse the dir's existing
      session via `GetByDir`. Test: opening a folder twice reuses the same session. protocol.md
      `spawn_at` updated. Cleanup of the EXISTING pileup is now a manual step — Phase 1 made every
      session individually visible and per-session deletable in the sidebar, so duplicates can be
      pruned there (no destructive auto-cleanup, since which to keep is the user's call).
- [x] 2026-07-07 — **Dev/Prod naming divergence resolved by dropping the toggle** (tail of Phase 4).
      The temporary Dev/Prod server toggle (which kept two registries, so one `session_id` could
      carry a different name per server) was removed in `a2a4c48`; the app now targets a single
      configurable server URL. Cleaned up the last stale "Dev/Prod" comments (`SettingsStore.kt`,
      `VoiceController.kt`, `gateway/ops.go`) to refer generically to switching servers. Stable
      `session_id` identity still lets the app re-attach to the same session across any two servers
      that name it differently.

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
- [ ] Validate the audio `codec` field and reject unknown values (`audio.go`) instead of silently
      treating them as PCM16.
- [x] 2026-07-05 — Loud startup warning when `SPAWNER_ROOT` is empty (unrestricted spawn scope)
      (`main.go`).
- [ ] Graceful shutdown waits briefly for an in-flight turn instead of a hard 5s HTTP-server kill.

## Done

- [x] 2026-07-06 — **Start a new project in a non-existing folder from the sidebar picker.** The
      New-session browser could only spawn in folders that already exist. Added a "New project
      folder here…" action (below "Start session here") that prompts for a name, creates the folder
      under the currently-browsed directory, and attaches. Server: `spawn_at` gained an optional
      `create` flag — `doSpawnAt` `mkdir`s the (root-jailed) path first, erroring `bad_path` if it
      already exists or escapes the roots. Android: `Outbound.spawnAt(create=)` + `spawnNewFolder`
      + the picker dialog. `docs/protocol.md`; `TestSpawnAtCreatesNewFolder` / `TestSpawnAtCreateJailed`.
- [x] 2026-07-06 — **Fuzzy-match confirmation in the spawn dialog.** When navigating to a leaf
      project lands on a folder whose name carries a token the user never said — the matcher
      stretched "mail" onto `mail_play` because no `mail` folder exists — the flow no longer
      silently attaches; it asks a new `[await_confirm]` state ("did you mean mail_play?") first.
      "yes" proceeds to the target/attach question, "no" backs up to the parent's folder list.
      Exact names and multi-word names spoken in full ("mail play" → `mail_play`) skip it; only
      leaf commits confirm (a stretch onto a root/namespace just keeps browsing). `descend` now
      returns an `inexact` flag (`landedExact` = every folder-name token was spoken, exactly or via
      a fuzzy slip). `gateway.dialog` + `docs/commands.md`; `TestSpawnFuzzyMatchConfirm` /
      `TestSpawnExactMatchNoConfirm`.
- [x] 2026-07-06 — **Android mTLS client certificate.** Completes the auth-hardening epic on the app
      side: the phone can now present a client certificate to a mutual-TLS server (`SPAWNER_TLS_CLIENT_CA`).
      New `net/ClientTls.kt` builds an OkHttp `SSLSocketFactory` + `X509TrustManager` from a PKCS#12
      keystore (server still verified against the system trust store — only a client key is added);
      `SpawnerClient` takes an optional `ClientTls` and applies `sslSocketFactory(...)`. `SettingsStore`
      persists the imported `.p12` in private storage + its passphrase (`importClientCert`/`clearClientCert`/
      `hasClientCert`). `VoiceController.connect` loads it when present and surfaces a load/passphrase error,
      falling back cert-less. UI: **Settings → Server → Client certificate (mTLS)** — SAF `.p12` import
      (`rememberLauncherForActivityResult` + `OpenDocument`), passphrase field, Remove. APK built +
      installed on the Pixel 8a. README security section updated.
- [x] 2026-07-06 — **Sessions drawer auto-refreshes on open + pull-to-refresh.** Opening the drawer
      now calls `controller.discover()` (folded into the existing `drawerState.targetValue == Open`
      effect), and the session list is wrapped in a Material3 `PullToRefreshBox` so pulling down
      refreshes it; the spinner clears when a fresh list lands or after a 1.5 s cap (discover is
      fire-and-forget and an unchanged list won't re-emit). The `⟳ Refresh` button is gone.
      `MainActivity.kt`; APK built, installed on the phone.
- [x] 2026-07-05 — **Delete clears every same-dir record; no more ghost sessions.** The sidebar
      collapses same-directory sessions to one row, so a second registry record for a dir (born when
      `uniqueName` appends `-2`) was invisible but still owned a name — blocking a rename onto it.
      `doDeleteDiscovered`'s no-transcript branch only dropped the single record matched by
      session_id, stranding same-dir siblings as ghosts. Refactored to resolve the directory (from
      transcript or record) and delete **every** registry record for it via new `deleteRecordsForDir`
      helper. `TestDeleteDiscoveredClearsSameDirGhosts`. Cleared two live ghosts (`claude_spawner`,
      `claude_home-2`) via a `delete`-by-name to the running server (no restart).
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
- [x] 2026-07-07 — **Fix: reconnect catch-up only pulled the newest page, leaving a middle gap.**
      The every-reattach refetch above requests only the most recent history page (30 transcript
      entries), so a long detach/disconnect — and agentic turns burn many entries each — left a hole
      between what the app still held and that newest page; the missing middle only reappeared on
      manual scroll-back. Now `onHistory` records the highest index we already held and, on a top
      reload, auto-pages older (via the shared `fetchOlder`) until it reconnects with that watermark
      (or hits the transcript start), so the whole away-gap backfills on reconnect. (`VoiceController.kt`.)
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
      servers are wired in by the live broker deployment (`docker-compose.broker.yml`).
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

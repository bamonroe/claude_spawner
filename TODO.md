# TODO — claude_spawner

The **live task list** for active and recently-completed work. This is the single source of
truth for what's in flight; `README.md` keeps the historical phase-by-phase roadmap.

**Maintenance rule** (see `CLAUDE.md`): edit this file in the same commit that proposes or
completes a feature **or a test**. Adding a feature/test → add an unchecked box here. Finishing
one → check it off (move to _Done_, dated). Dropping a test/feature → remove it with a one-line
why. A change that leaves this file stale is incomplete.

Dates are `YYYY-MM-DD`.

## Active

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

- [x] 2026-07-14 — **Fix: hands-free Bluetooth mic needed toggling to engage.** The SCO-failure
      latch only cleared when the input *value* changed, so re-tapping "Headset" was a no-op (had to
      bounce Device→Headset to retry); now any explicit input pick clears it. And `verifyHeadsetMic`
      judged the link with one check at 2.5 s and latched failure — too quick for car kits / some
      earbuds — so it now polls ~6 s and succeeds the moment SCO goes live
      (`VoiceController.setAudioInput` / `verifyHeadsetMic`).

- [x] 2026-07-14 — **Feat: adaptive noise-floor VAD + optional headset noise suppression.** The
      hands-free endpointer tracked no ambient floor, so a fixed energy gate either passed noise or
      (raised) rejected quiet speech in noisy rooms. `Endpointer` now tracks the noise floor from
      non-speech frames and lifts the onset bar to a multiple of it, never below the configured
      threshold (quiet rooms unchanged); default on, Settings → Audio "Adapt to background noise",
      the mic slider becomes a floor. Also split the platform NoiseSuppressor from AEC so the
      headset/media path can opt in independently ("Headset noise suppression", off by default).

- [x] 2026-07-14 — **Fix: turn reply silently dropped when the client stalls mid-turn.** A turn's
      reply reached the app only if the phone's socket accepted the write at `finish` time. If the
      phone was briefly unreachable then (backgrounded / a mobile stall past `c.send`'s write
      deadline), the write failed and the reply was buffered **undelivered** — and the *next* turn's
      `startTurn` (and background auto-compress) wiped that buffer before it was redelivered, so the
      answer never appeared even though the server logged it. A slow local model made it common by
      widening the stall window, but it was model-agnostic. Fix (`internal/gateway/jobs.go`): a new
      `beginTurn` preserves an undelivered `j.final` across turn starts (only drops it once
      delivered); `flushPending` redelivers it at the next turn's first write (thinking ping), by
      which point a stalled socket has usually recovered; `finish` clears the buffer on a reaching
      write and marks it undelivered otherwise; and `bindJob` now hands back a buffered reply on
      reattach **independently** of whether a new turn is running (the old running-first `switch`
      skipped it). Regression test `drop_repro_test.go` (`TestBufferedReplyRedeliveredAfterFailedSend`)
      drives the real turn machinery with a stalled→recovered sink. `docs/protocol.md` `output` note
      updated. Root cause was found while debugging why an opencode/Ollama "website" session's
      follow-up replies never showed in the app.

- [x] 2026-07-13 — **One-shot voice spawn with defaults.** "hey buddy, new session [called <name>]
      [in <location>] [on <provider>] [with <profile> profile]" now creates + attaches immediately,
      defaulting each unspoken part: location → the user's **home directory**, provider → the default
      backend (Claude), profile → the marked-default profile. The command parser gained `Intent.Name`
      + `Intent.Profile` and `extractSpawnProfile`/`beforeAny` (name via "called"/"named" bounded by
      the location prep; profile via "with <name> profile" or "profile <name>"). The gateway's new
      `spawnCommand` fast-path resolves the location (home default) and, when it pins a concrete
      folder (not a root/namespace, not a fuzzy guess), calls `doSpawnAt` (now taking a custom name +
      announce flag, returning the session) to create + attach; it falls back to the browse dialog for
      a new *project*, an unresolved/fuzzy location, or a container of sub-projects. Fast-path dirs are
      always under `SPAWNER_ROOT`, so the voice jail holds. Registry aliases + `docs/commands.json`
      regenerated; `docs/commands.md`, `README.md` updated. Parser + gateway tests green. Not yet
      tap/voice-tested on the phone against the live server.

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

- [x] 2026-07-14 — **Live model auto-discovery per backend.** A backend can now report its runnable
      models at runtime instead of only from a compiled list, so no rebuild/APK update is needed when
      a model changes. Generic two-mechanism seam on `agent.Agent`: the compiled `Models` slice is the
      **fallback**, and optional `DiscoverArgs` + `ParseModels` declare a probe whose stdout is the
      **live** catalogue; `Agent.Catalog()` shadows the fallback with discovered models everywhere the
      list is read (resolution, provider overlay, `agents` message). `Driver.RefreshModels` runs each
      probe over the host SSH pool — at boot before the provider overlay validates, and throttled on
      each client connect (re-broadcasting `agents`). opencode is the first user: `opencode models
      ollama` → `ollama/*` models, aliases as the bare model id (fallback aliases realigned to match).
      Claude/Codex keep their compiled lists. Getting a model *into* opencode stays the user's job
      (`ollama pull` + `opencode.jsonc`); the server only auto-surfaces what opencode reports. Parser +
      catalog/fallback tests; `go test ./...` green; architecture + README + TODO updated. Server-only
      change — no APK needed (the app already renders whatever `agents` advertises).

- [ ] **Session execution-environment profiles** — named, per-session, templatable bundles of
      mounts / credential injection / network endpoints for host + sandbox turns, replacing the flat
      global `SPAWNER_SANDBOX_*` config (which becomes the built-in `default` profile). Prerequisite
      for the opencode backend and for reaching a local Ollama model across hosts/sandboxes. Full
      design + phasing in `EXEC_PROFILES_DESIGN.md`. Proposed 2026-07-13.
      - [x] 2026-07-13 — Server foundation: added `ExecProfile` + registry, optional
        `SPAWNER_PROFILES` (`profiles.json`) loader, durable `Session.Profile`, and driver
        resolution. The built-in `default` profile is seeded from the existing sandbox env vars, so
        sessions with no profile behave exactly as before. Profile env now reaches host/SSH turns and
        host-side short commands; sandbox profiles can override image, mounts, credential mounts, env,
        and run args when the persistent container is created. Covered by focused session/gateway
        tests. Documented `locked`/`open` example presets now ship in
        `deploy/profiles.example.json`, guarded by a loader test.
      - [x] 2026-07-13 — `{{.Var}}` templating: every string-bearing profile field is rendered per
        turn (in `Driver.ProfileFor`) against built-ins `{{.Home}}`/`{{.Session}}`/`{{.Dir}}` plus a
        user-defined `{{.Vars.X}}` map — global `SPAWNER_PROFILE_VARS` (JSON) overlaid by the
        profile's own `vars`, profile winning on a clash. An undefined var is a hard error that
        surfaces on the turn. Unlocks Ollama-across-hosts (see the `ollama` preset). Covered by render
        + merge + fail-loud tests.
      - App-managed profiles registry (user-defined profiles + default marker). Full design in
        `EXEC_PROFILES_DESIGN.md` phase 6.
        - [x] 2026-07-13 — Server foundation: `ProfileRegistry` is now a file-backed store (path +
          mutex + atomic flush, `Put`/`Delete`/`SetDefault`/`Get`/`DefaultName`) mirroring
          `HostStore`. "Default" is a per-profile marker (no built-in `default` profile); resolution
          falls back to the marked profile else the first; an empty `image` falls back to
          `SPAWNER_SANDBOX_IMAGE`. First run seeds `bare-metal`/`sandbox`/`locked` from the sandbox
          env vars and persists them. Store CRUD + seeding + default-marker + example-load tests green.
        - [x] 2026-07-13 — Wire + gateway CRUD: `profile_put`/`profile_delete`/`profile_set_default`
          handlers (`gateway/profiles.go`) mutate + `broadcast(msgProfiles)`; `msgProfiles` now emits
          the full ExecProfile per entry; `bad_profile` error; `docs/protocol.md` + docsync/clientsync
          + `Protocol.kt` (`profilePut`/`profileDelete`/`profileSetDefault`, enriched `ProfileInfo`).
          Gateway CRUD-broadcast test green.
        - [x] 2026-07-13 — App profiles settings page: `ProfilesSettings` Compose screen (list +
          per-row default marker/"Make default" + Edit/Delete; add/edit form with name, target chips,
          image, home_mount, multiline mounts/creds/env/run_args/vars), `ProfilesController` interface
          + impls in `VoiceController`/`WebAppController`, `set_profiles` hub row, routing in both
          clients. Both platforms compile; APK built + installed on the emulator; screen renders +
          navigates. Built with a Temurin-17 JDK copied from the `android-emulator` container (box's
          `~/.gradle/gradle.properties` pins a removed JDK 21; its JDK 25/26 are too new for Gradle
          8.10.2 — flag for the user to fix). Remaining: form + CRUD round-trip on a server-connected
          client (emulator was offline) and a Pixel 8a install.
      - [x] 2026-07-13 — Protocol/client advertisement slice: server now pushes a `profiles`
        message after `agents`, carrying each profile's `name` and advisory `target` plus default
        name. Android and web parse and retain it on `AppController.profiles`.
        `docs/protocol.md`, docsync, gateway test, and shared Kotlin compile gate are green.
      - [x] 2026-07-13 — Profile selection wire slice: `spawn_at.profile` now persists a
        non-default profile on the session, `attached` / `session_list` / `discovered` echo it, and
        Android plus web controllers forward the optional field. The server also honors a selected
        profile's advisory target when `spawn_at.target` is omitted.
      - [x] 2026-07-13 — New-session profile picker: `BrowseScreen` now shows execution-profile
        chips when more than one profile is advertised, defaults to the server's first/default
        profile, applies the profile's advisory host/sandbox target on selection, and sends the
        chosen profile for both "start here" and "new folder" spawns.
      - [x] 2026-07-13 — Profile-scope the sandbox home mount (found in the 2026-07-13 review).
        Added a `home_mount` field to `ExecProfile`; `createArgsFor` mounts the host home only when
        the resolved profile carries it. The `default` profile is seeded from the server's `HOME`
        (unchanged behavior), so a `locked` profile with empty `home_mount`/`mounts`/`creds` now gets
        no host home inside the box. Covered by a new executor test.

- [ ] **Kokoro server-side TTS** — synthesize reply speech on the server (Kokoro-82M via
      [Kokoro-FastAPI](https://github.com/remsky/Kokoro-FastAPI), OpenAI-compatible
      `/v1/audio/speech`, streaming, CUDA) and ship audio to clients, replacing on-device
      Android `TextToSpeech` and the browser's `SpeechSynthesis` with one consistent
      high-quality voice — and giving the web client a real voice for the first time.
      Scoped 2026-07-12; design notes:
      - **Pull, not push**: the speak/mute/summary-only decision stays client-local (as today —
        the server has no per-client speak flag). Client sends a new `speak` request
        (`{id, text}`, markdown already stripped client-side via `tts/Markdown.kt`); server
        streams back synthesized audio. No audio is synthesized for muted clients.
      - **Wire**: first server→client binary audio. Frame it like the reverse of the mic path
        (`audio.go`): a `speak_audio {id, codec}` JSON header, then binary WS frames, then
        `speak_end {id}`. Client→server binary today is implicit (one stream type); server→client
        needs the id-tagged header since multiple speaks can be in flight. `docs/protocol.md` +
        docsync/clientsync exemptions updated in the same pass.
      - **Server**: new `internal/tts` package mirroring `transcribe/remote.go` (HTTP POST to
        `SPAWNER_TTS_URL`, empty = disabled → clients fall back to local TTS). Env vars:
        `SPAWNER_TTS_URL`, `SPAWNER_TTS_VOICE`, `SPAWNER_TTS_FORMAT` (opus default). New compose
        service `kokoro` — **shares the GPU with whisper** (decided 2026-07-12; room to spare:
        ~2–3 GB VRAM at inference, model <1 GB).
      - **Android**: stream into a `MODE_STREAM` AudioTrack in `Speaker.kt` (the warm-track beep
        machinery and `AudioRouter` earpiece/speaker/headset routing already exist); on-device
        TTS remains the fallback and a settings toggle picks server-vs-local voice. Barge-in
        (`stop`) kills playback + in-flight synth like today's `Speaker.stop()`.
      - **Web**: decode via Web Audio (`decodeAudioData` on the existing AudioContext in
        `WebAudio.kt`); replaces SpeechSynthesis when the server offers TTS.
      - **Latency**: v1 synthesizes the final reply + `say` lines (Kokoro-FastAPI streams
        sentence-by-sentence, so first audio is fast); live chunk-by-chunk speech of streaming
        prose stays on-device initially, revisit after latency is measured.
      - **Voice picker** (decided 2026-07-12): a dropdown in the app's **audio settings tab**,
        fed by the server relaying Kokoro's `/v1/audio/voices` list — same pattern as the
        whisper-model picker (server-supplied options in settings; free default from
        `SPAWNER_TTS_VOICE` until the user picks).
      - Milestones: (1) ✅ 2026-07-12 compose service + `internal/tts` + config/docs, CLI-tested —
        `kokoro` service live on the GPU (~810 MiB VRAM alongside whisper), 68 voices, opus
        synthesis verified via curl + the Go client against the running server; model persists in
        the `kokoro-models` volume; gateway health-check logs the voice count at startup
        (`SPAWNER_TTS_URL` set in the live env, takes effect on next rebuild);
        (2) ✅ 2026-07-12 `speak` protocol + gateway plumbing + drift tests — client sends
        `speak {id, text, voice?}`, server streams `speak_audio {id, codec}` + binary frames +
        `speak_end {id, error?}` (a per-connection ordered worker, so streams never interleave);
        `hello_ok` gains a `tts` capability flag; refusals (disabled/blank/queue-full) come back
        as an error-bearing `speak_end` so clients fall back to on-device TTS. Documented in
        `docs/protocol.md`, clientsync exemptions carry the M3/M4 pointers. Verified live on a
        scratch server: 44.8 KB opus streamed end-to-end over the WebSocket;
        (3) ✅ 2026-07-12 Android playback + settings toggle + fallback — `speak` gained an
        optional `format` override (Android asks for `pcm`, raw 24 kHz s16le mono, and streams
        the binary frames into a MODE_STREAM AudioTrack on a dedicated worker in `Speaker.kt`;
        no decoder). Routing decision stays client-local in `speakText` (VoiceController): the
        "Server voice" audio-settings toggle (default on, enabled only when `hello_ok` says
        `tts`) picks Kokoro, and any error-bearing `speak_end` falls back to on-device TTS.
        Barge-in/mute/disconnect cancel in-flight speaks and silence the stream; the clientsync
        speak exemptions are removed (Kotlin wire strings exist now);
        (4) ✅ 2026-07-12 web playback — the browser asks for `mp3` (decodeAudioData wants one
        complete clip and mp3 decodes everywhere incl. Safari), accumulates the binary frames,
        and on speak_end decodes + queues the clip on a shared AudioContext so utterances play
        in order (WebAudio.kt server-TTS section); same speak() router/fallback/cancel shape as
        Android in WebAppController, and the hands-free VAD echo-triple + SPEAKING pill treat
        server playback like SpeechSynthesis;
        (5) ✅ 2026-07-12 voice picker + barge-in polish — new `tts_voices` request relays
        Kokoro's catalogue live (server-default first; picking a voice speaks a preview in it;
        the choice is client-local, riding each speak's `voice` field), and new `speak_stop`
        drops the connection's queued speaks and aborts the in-flight synthesis via a
        per-request cancel (its stream closes with a `cancelled` speak_end) — sent by both
        clients alongside local barge-in/mute. Remaining: hear it on the phone after the next
        rebuild ships the server TTS code (the feature is default-on; flip the Server voice
        switch off to compare).

- [ ] **Per-session record locking** (1.0 quality pass, deferred item). The store hands out
      shared `*session.Session` pointers; a running turn's goroutine mutates the record
      (Started/PendingSeed/primes + Put) while another device's read loop can mutate it too
      (kill-job, set_agent/model, rename). The one-writer invariant covers turn-vs-turn and
      reconcile, but not turn-vs-ops or two devices. Wants a deliberate design (per-record
      mutex or store-mediated mutation), not a drive-by — scoped out of the 2026-07-13
      concurrency fixes below.

- [x] 2026-07-13 — **Gateway concurrency fixes** (the 1.0 quality pass, part 2). Verified the
      audit's claims against the code and fixed the real ones: `reconcileJobs` held pointers into
      `sess.Jobs` across appends/drops (a realloc could lose the Notified flag → finished jobs
      re-announced forever) — now index-based with deferred drops; `conn.closed` was read by job
      sinks without synchronization — now rides under `wmu`; `NotifyShutdown` read `c.attached`
      from outside the read loop — now via the new locked `attachedSession()` reader (writes go
      through `setAttached`). Documented the conn goroutine model (read-loop ownership, the three
      locked exceptions, job-hub lock ordering) on the struct. Verified-clean (no change): speak
      streaming (deferred Close covers all paths), SSH pool (dials under the pool lock), job hub
      lock ordering. `go test -race ./internal/gateway` passes.

- [x] 2026-07-13 — **opencode backend (local Ollama)** — the Phase 7 spike, landed. New
      self-contained `internal/agent/opencode.go`: `opencode run --format json`, self-assigns its
      `ses_…` id (adopted from the stream), resumes with `-s`, `--auto` = bypass, `--` terminates
      flags. Parser keys off each event's `part.type` — text parts concatenate into the reply (live
      via `OnText`, synthetic/ignored skipped), tool parts become breadcrumbs (filePath from the
      tool input), the step-finish part carries usage; a top-level error event fails the turn while
      still returning the session id. Models are the `ollama/*` catalogue (`qwen` = qwen2.5-coder,
      `llama`) resolved via the provider block in the host user's `~/.config/opencode/opencode.jsonc`
      (Ollama at `localhost:11434/v1`), so turns run entirely on local weights. Registered in
      `agent.Default()`; per-target binaries `SPAWNER_SSH_OPENCODE_BIN`/`SPAWNER_SANDBOX_OPENCODE_BIN`
      wired into `Driver.AgentBins`; `opencode` added to `spawnAgentWords` + the STT command vocab.
      Parser + error tests green; build/vet/test + docsync/clientsync clean. Docs updated
      (`README.md`, `docs/architecture.md`, `CLAUDE.md`). Verified `opencode run` end-to-end against
      the live Ollama server (both models reply; `-s` resume works); not yet exercised through a
      spawned spawner session on the rebuilt server.
      - [x] 2026-07-13 — Native opencode transcript reader (history replay + context badge). opencode
            persists sessions in a SQLite DB, not files, so `opencodeFS`
            (`internal/session/opencode_transcript.go`) shells out to `opencode export <id>` (mapping
            its message/part JSON to `[]Message`, taking context size from the last `step-finish`
            tokens since session-level `info.tokens` is summed across turns) and `opencode session
            delete <id>`, over the same SSH seam as the file readers. New `TranscriptOpencode` kind;
            `transcriptReaderFor` routes to it. Ids are `ses_`-validated before shell interpolation.
            Pure map/context/id-validation unit tests green. **Caveat:** the reader assumes the host
            `opencode` binary is on PATH (no config handle for `SPAWNER_SSH_OPENCODE_BIN`); still to
            exercise reattach through a live spawned session.

- [x] 2026-07-13 — **AI backends made fully self-contained** (the 1.0 quality pass, part 1).
      Each backend now owns its stream parser (`Agent.ParseTurn`) and declares its transcript
      layout (`Agent.Transcript`), replacing the session driver's `Format` switches — `Turn` has
      no per-backend branching left. `internal/agent` restructured: one file per backend
      (`claude.go`, `codex.go`, each the full backend: entry + args + parser + tests), shared turn
      vocabulary in `turn.go` (`ToolUse`/`Usage`/`RateLimit` moved from session, aliased back).
      New "Adding an AI backend" checklist in `docs/architecture.md` — a Gemini/local backend is
      one new file plus registration + env wiring.

- [x] 2026-07-13 — **Whisper anti-hallucination: server-side Silero VAD + non-speech-token
      suppression.** Whisper filled silent stretches in push-to-talk/hands-free clips with looped
      YouTube-outro phrases ("Thanks for watching…"). All three whisper images now bake in the
      Silero VAD model (pinned HF revision) and run `whisper-server` with `--vad --vad-model …
      --suppress-nst` as entrypoint defaults, so silence is stripped before decoding and non-speech
      tokens are suppressed for every request — no gateway change needed. Documented in
      `whisper/README.md` + `docs/architecture.md`.

- [x] 2026-07-12 — **Collapsed spawner-server host mounts.** With SSH-native turns, host FS access
      already went over SSH; trimmed the broad `${HOME}`/`/data`/`passwd` bind mounts down to
      essentials (`state` + the narrow whisper models dir). All four steps below done.
  - [x] 2026-07-12 — **Whisper models → `/data/storage/whisper`**, mounted directly (rw into the
        gateway for on-demand downloads, ro into the whisper service at `/models`) instead of via the
        broad `${HOME}` mount. Severs whisper from `${HOME}`.
  - [x] 2026-07-12 — **Restart via the in-process Go SSH pool** instead of shelling to `openssh`.
        The gateway runs `SPAWNER_RESTART_CMD` (now the bare `setsid …` host command, no ssh wrapper)
        on the host over its own connection pool; drops the `/etc/passwd:ro` mount and `openssh-client`
        from the image (both existed only for the restart client). Falls back to local `sh -c` with no pool.
  - [x] 2026-07-12 — **Bake the web bundle into the image** at `/srv/web` (served over host
        networking), so one artifact ships API + client with no host mount for the web dir.
        `rebuild-container.sh` stages the Gradle output (`:app:wasmJsBrowserDistribution`, built
        out-of-band) into the build context; the Dockerfile `COPY`s it. `SPAWNER_WEB_DIR=/srv/web`.
        Behavior change: a UI change now needs a container `rebuild` to ship (a `bounce` won't).
  - [x] 2026-07-12 — **Route sandbox + discovery through the SSH pool, then remove the `${HOME}`/`/data`
        mounts entirely.** The last coupling was sandbox-target sessions reading their transcript /
        stat+mkdir'ing their spawn dir in-process (host `""`). `claudeFSFor("")`, `dirExists`/`makeDir`,
        and `DiscoverSessions`/`TranscriptPathByID`/`TranscriptCwd` now map the empty host to loopback
        and read over SSH — the sandbox's podman already runs there, so its files live on the host.
        Also made SSH-native **unconditional** (dropped the `SPAWNER_SSH` toggle; `HostExecutor` + the
        local-child-process paths survive only as the hermetic unit-test executor). `docker-compose.yml`
        now mounts only `state` + `/data/storage/whisper`. Follow-up: the old `~/.local/share/whisper`
        model copies can be deleted once verified live.
  - [x] 2026-07-12 — **Deploy portability + full doc pass.** De-hardcoded the deploy off the
        maintainer's machine: `rebuild-container.sh` derives the repo root from its own location and
        the target user from the repo owner (`SPAWNER_DEPLOY_USER` overrides); parameterized the
        whisper models dir in `docker-compose.yml` as `${SPAWNER_WHISPER_MODELS_DIR}`; synced the env
        templates + added a first-run guide (whisper models, sandbox image) to `deploy/README.md`.
        Fixed the Codex-in-sandbox gap (mount `~/.codex` + `SPAWNER_SANDBOX_CODEX_BIN`) and documented
        it. **Removed the vestigial `SPAWNER_CODEX_BIN`/`cfg.CodexBin`** (written once, never read —
        superseded by `SPAWNER_SSH_CODEX_BIN`). Swept all docs for stale "home/roots mounted" and
        `SPAWNER_SSH` toggle references (README, `server/Dockerfile`, `docs/architecture.md`) — the
        server mounts no host home; everything reads over SSH.
  - [x] 2026-07-12 — **Fresh-clone runnability pass.** Simulated a new user cloning + following the
        docs; fixed every hard failure found: dropped `depends_on: whisper` (the documented text-only
        `up … spawner-server` dragged in the GPU-requiring whisper service), de-personalized the env
        template (placeholder `you` for user/home, `EDIT EVERY VALUE` header, sandbox target ships
        DISABLED, restart cmd path is an explicit edit-me), server now **rejects the template token
        `change-me`** at startup, fixed whisper/README's phantom `whisper-fast` compose service,
        added a Prerequisites section (sshd is load-bearing; claude/codex logged in) + "Getting a
        client" to deploy/README, fixed the root-owned-bind-mount `mkdir` ordering trap, corrected
        the compose header's stale "seed known_hosts" claim, root README got a Quick start pointer,
        android/README rewritten for the KMP layout (SDK bootstrap, Ktor not OkHttp, real
        default-URL story, `SPAWNER_DEBUG_KEYSTORE`, emulator marked maintainer-specific), and JDK
        guidance standardized to 17+ (web-client.md's hardcoded JAVA_HOME dropped).
  - [x] 2026-07-12 — **Web bundle auto-builds on first deploy.** `deploy/rebuild-container.sh` now
        builds the Wasm bundle itself when it's missing — in a throwaway `gradle:8.10.2-jdk17`
        container with an isolated Gradle home (`~/.cache/claude_spawner-gradle`, so host
        `gradle.properties` can't leak a bad JDK path in) — so a fresh clone's first
        `rebuild-container.sh` ships the browser client with no host JDK/SDK and no manual Gradle
        step. Non-fatal on failure (server still deploys, just client-less). Verified: the exact
        containerized command built the bundle from a cold cache.
  - [x] 2026-07-12 — **Code-level vestigial-organs sweep** (three parallel audits: Go server,
        Kotlin client, all docs vs code). Result: docs clean; Go had one stale comment
        (`ssh_test.go` referencing the removed `SPAWNER_SSH` toggle — reworded); Kotlin had four
        unused imports in `MainActivity.kt` — removed. The "Porcupine" mention the user saw on
        GitHub is **master's** README (this branch fixed it; master is ~167 commits behind —
        merge/default-branch switch pending user decision).
- [x] 2026-07-12 — **Restart button: optional rebuild.** The restart dialog has a *Rebuild from
      source* checkbox (default on). The `restart` message carries a `rebuild` flag (nil/absent =
      rebuild, back-compat); the server substitutes the `%REBUILD%` token in `SPAWNER_RESTART_CMD`
      with `rebuild`/`bounce` and passes it to `rebuild-container.sh` — `rebuild` does the `--no-cache`
      recompile, `bounce` recreates from the existing image (fast, no code change). Also hardened the
      script: force-remove the stale-named container before recreate (cross-project name collision was
      silently no-op-ing the recreate) and always `--no-cache` on rebuild (stale-layer reuse shipped an
      old binary in a fresh container). Wired through both clients; docs + protocol updated.
- [x] 2026-07-12 — **Whisper model download-on-select.** The audio picker offers the full curated
      English catalogue (`transcribe.EnglishModels`); picking a model that isn't on disk downloads it
      from Hugging Face into `SPAWNER_WHISPER_MODELS_DIR`, shows a live progress bar, then hot-loads it
      — a fresh deploy auto-fetches the boot model, so no manual ggml placement. New `whisper_download`
      progress broadcast + `whisper_models_local` field; dropdown marks not-yet-downloaded models with
      a ⤓ and a download bar. Wired through both the Android and browser clients. Installed on the
      Pixel 8a + tablet; server-side takes effect on the next container rebuild.
- [x] 2026-07-12 — **Server comes up bare: self-managed SSH keypair + auto-seeded loopback trust.**
      The server now mints its **own** ed25519 keypair (separate from the host's `~/.ssh` keys) on
      first boot when `SPAWNER_SSH_KEY` is empty — at `<state>/ssh/id_ed25519`, writing the public key
      to `<key>.pub` and logging it — and auto-trusts the loopback host key into its own known_hosts
      at startup (best-effort TOFU), so no manual key placement or `ssh-keyscan` seeding is needed.
      The one manual step to enable host turns + the restart button is documented: add the generated
      public key to the host user's `~/.ssh/authorized_keys`. New `session.EnsureServerKey` (+ test);
      wired in `main.go`; `SPAWNER_SSH_KEY` default flipped to empty in the container env; docs updated
      (`deploy/README.md`, `README.md`, `CLAUDE.md`, `deploy/spawner-container.env.example`). The
      loopback entry (the local machine running the container) is the seeded `localhost` host.
- [x] 2026-07-12 — **Pooled the gateway + whisper into one compose stack; a single `docker compose
      up -d --build` launches the whole backend.** Merged `deploy/spawner-container.yml` into the root
      `docker-compose.yml` as a second service (`spawner-server` alongside `whisper`) and deleted the
      standalone file. Added a git-ignored root `.env` (+ committed `.env.example`) holding
      `SPAWNER_UID`/`SPAWNER_GID` so the bare command runs the server as you with no prefix.
      `rebuild-container.sh` (the restart button) now scopes to `up -d --build spawner-server`, leaving
      whisper untouched. Updated `deploy/README.md`, `README.md`, `docs/architecture.md`, and the
      compose/script headers. `docker compose config` validates; no env-var or behavior change.
- [x] 2026-07-12 — **Removed the bare-metal/systemd deployment remnants; the container is the one
      supported route.** The server now runs only as the Docker container that builds the Go binary
      and drives the host over SSH (`deploy/spawner-container.yml`). Deleted the systemd-only
      artifacts (`deploy/spawner-server.service`, `deploy/spawner-server.env.example`,
      `deploy/rebuild.sh`) and rewrote the docs/comments that framed a bare-metal path: `deploy/README.md`
      (container-only), `README.md` (live-deployment + build sections), `CLAUDE.md` (restart var +
      build/run note), `docs/architecture.md` ("runs in a container, driving the host over SSH"),
      `docs/protocol.md` (`restart` message), root `docker-compose.yml` header, `whisper/README.md`,
      `android/README.md`, `sandbox/README.md`, and the `systemctl`/`KillMode` code comments in
      `config.go`, `session.go`, `main.go`, `ops.go`, `executor.go`. No behavior or env-var change.
- [x] 2026-07-12 — **Audio picker redesign: independent Output + Input sections.** The top-bar
      audio button now opens a two-section picker — **Output** (Earpiece / Speaker / Headset /
      Mute — where the voice plays) and **Input** (Device / Headset — which mic listens) — chosen
      independently; picks keep the menu open so both set in one visit. The two explicit choices
      fully determine the capture route with no inference: `VoiceController.resolveMicProfile` now
      keys off `AudioInput` × `AudioOutput` instead of guessing the mic from the output. New shared
      **`AudioInput`** enum; retired **`AudioOutput.BLUETOOTH`** (its "use the whole headset" meaning
      is now Output=Headset + Input=Headset — migrated on load from the legacy `audioOutput=bluetooth`
      pref). The old Settings → **Hands-free microphone** toggle is removed (the picker's Input
      section replaces it). BT-permission prompt moved from output→input selection. Web hides the
      Input section (empty `audioInputs`). Touches `AudioInput`/`AudioOutput`, `AudioRouter`,
      `VoiceController`, `TopBar`, `MainScreen`, `MainActivity`, `WebRoot`, `SettingsScreens`, docs.
- [x] 2026-07-12 — **Fix: sidebar recency sort only applied to the orange tier.** The drawer sort
      ordered *only* busy/unread ("orange") sessions by most-recent activity; the neutral majority
      fell to alphabetical, so a just-active session landed wherever its name sorted (often far from
      the top). Now `thenByDescending { lastActive }` orders every tier newest-first (attached still
      pinned top, orange still next), with name only as a tiebreak — the most recently active session
      is always highest in its group. Verified on the emulator against the live server (claude_spawner
      "just now" → trashbot 2m → life 1d → email 2d, top to bottom) and installed to the phone + tablet.

- [x] 2026-07-12 — **Fix: adopting a stale cached session mints a phantom `<dir>-2` duplicate.**
      On a fresh offline open the app shows the *previous* run's cached discovered row for a folder;
      tapping it sent `adopt` with a since-superseded `session_id`, and the server dutifully
      registered it as a second record, name-deduped to `claude_spawner-2`. `doAdopt` now checks for
      a live local session in the same dir first (`GetByDirHost(dir, LocalHost)`) and attaches to it
      instead of registering the stale id — the registry holds one local session per folder.
      Regression test `TestAdoptStaleIDReusesLiveSession`. Deleted the stray session that this
      produced.

- [x] 2026-07-12 — **Sidebar attention colour + sort.** The sessions drawer now colour-codes and
      sorts by attention (shared `commonMain`, so both the app and the web client): the **attached**
      session stays **purple** and pins to the top of its host group; sessions that are **thinking**
      (`busy`) or hold **unread output** are tinted **buddy orange** (the `BuddyOrange` accent, now a
      single shared const reused by the warm-cache indicator) and sorted next by most-recent activity;
      the rest stay neutral, alphabetical. Unread is tracked in-memory in `MainScreen` (seed each
      session at its current `lastActive` on first sighting, keep the attached one current → a session
      only turns orange when new output lands while you're attached elsewhere; opening it clears it).
      Verified on the emulator (purple/orange/neutral tiers + ordering) and installed to the phone +
      tablet.
- [x] 2026-07-12 — **Headset-media output + capture-follows-output fix** — the hands-free mic sat
      near the noise floor (~−60 dB, VAD never tripped) whenever the platform ran call-mode capture:
      `VOICE_COMMUNICATION` + AEC/AGC clamps a far-field voice, unlike push-to-talk's
      `VOICE_RECOGNITION`. Root cause was two coupled seams: (1) the output picker
      (`setCommunicationDevice`) and `resolveMicProfile` were decoupled — changing output didn't
      re-resolve/restart capture, so the mic got stranded in the wrong mode; (2) no explicit "media
      to headphones + built-in mic" output, so users tapped **Speaker** (forces comm routing, clamps
      the mic) trying to get high-quality playback. Fixes: new **`AudioOutput.HEADSET`** (shared
      enum) — full-quality media (A2DP) to headphones, built-in mic, `VOICE_RECOGNITION`, no
      AEC/NoiseSuppressor, no call mode (`AudioRouter.setOutput` clears the comm device); offered
      when headphones are connected and **auto-preferred** as the default (`onAudioRouteChanged`,
      init restore), still fully overridable via the picker; `setAudioOutput` now **restarts
      hands-free** so capture follows the pick; `NoiseSuppressor` gated behind the same flag as AEC
      so the media path captures raw far-field. Touches `AudioOutput`, `AudioRouter`,
      `VoiceController.resolveMicProfile/setAudioOutput/onAudioRouteChanged`, `HandsFreeRecorder`.
- [x] 2026-07-11 — **Detached background jobs that survive turns** — Claude's native
  `run_in_background` can't span the headless-resume turn boundary (the bg process shares the
  turn's process-group/pipes and dies at teardown; bg shells are tracked in-memory per claude
  process). New `spawner-job` wrapper (embedded, staged to each target) launches a command fully
  detached (`setsid nohup … </dev/null >log 2>&1 &`) with its own session/pgid, recorded in an
  on-target registry keyed by working **dir** (survives session_id rotation). `Driver.RunOnTarget`
  runs short commands on the session's target (host/SSH/sandbox); a turn-boundary + on-attach
  reconciler (`reconcileJobs`) notices finished jobs, injects a bounded completion note into the
  next dictation (`PendingNotes`), and primes Claude once per context (`JobsPrimed`) to use the
  wrapper. Reconcile/stage errors never block a turn.
- [x] 2026-07-11 — **Enforce the wrapper via a Claude PreToolUse hook** — priming alone relied on
  Claude's cooperation. The turn injects a `--settings` hook (`HookSettingsJSON` →
  `TurnSpec.SettingsJSON`) whose `Bash` matcher runs `spawner-job hook`, which **transparently
  rewrites** a `run_in_background` launch (PreToolUse `updatedInput`) into `spawner-job start '<cmd>'`
  — no cancellation, the same Bash tool just runs the wrapped command. Fires even under
  `--dangerously-skip-permissions`. Fallbacks keep enforcement: no jq → block (exit 2) with a
  redirect; unstaged wrapper → graceful no-op.
- [x] 2026-07-11 — **Human voice control for background jobs** — `hey buddy list jobs` / `kill job N`
  / `job status`: new `command.Kind`s (`ListJobs`/`KillJob`/`JobStatus`) + `Registry` entries +
  `docs/commands.{md,json}` regen, wired through `runCommand` to `Driver.RunOnTarget` running
  `spawner-job list`/`kill` (added a `kill` subcommand that group-SIGTERMs a running job). `kill job`
  requires a number so it can't collide with kill-session or abort-turn. Parse-disambiguation tests
  guard the collisions.

- [x] 2026-07-11 — **Curatable command tray: pick which commands the swipe-up shows** — the tray was
      hard-wired to every argument-free command. Now it's user-curated. In **Settings › Commands**
      each command is a **collapsible card** (tap the header to expand); an expanded card shows the
      spoken aliases, the alias editor, and an **Add to / Remove from tray** toggle (with a ★ marker
      on the header when it's in the tray). Argument-taking commands (`attach`/`kill`/`spawn`) show a
      note instead — a tap button can't supply a `<name>`. Selection persists in a new `trayCommands`
      pref (`Prefs` + both backends), seeded to the previous all-argument-free set, and the swipe-up
      `CommandTray` renders just those (empty tray → a hint pointing back at settings). `Prefs.kt`,
      `SettingsScreens.kt`, `InputBar.kt`, `MainScreen.kt` + both call sites.
- [x] 2026-07-11 — **Backend-aware full delete: wipe every on-disk trace of a session** — deleting a
      Claude session used to drop only its transcript `.jsonl`, leaving the `projects/<dir>/<id>/`
      sidecar (subagent logs + cached tool results) and the per-session state dirs (`tasks`,
      `file-history`, `session-env` under `~/.claude`) orphaned on disk. `claudeFS.deleteByIDs` now
      routes through `purgeByID`, which removes the transcript, its sidecar, and those state dirs
      (UUID-validated before any path/shell interpolation; works local and over SSH).
      `deleteForDir` purges the same way per session. `codexFS` overrides `deleteByIDs` to just
      remove the rollout `.jsonl` (Codex keeps the whole thread there — no sidecar/state). Test:
      `claudefs_test.go`.
- [x] 2026-07-11 — **Fix: phantom "/data" session keeps reappearing** — the account-global
      `/usage` probe (`Driver.Usage`) ran `claude` with `cwd = SpawnRoots[0]` (e.g. `/data`),
      leaving a transcript under `~/.claude/projects/-data/` on every run. Session discovery
      surfaced that transcript as a session; the normal delete only drops the store record, so the
      next probe re-created it. Fixed by giving the probe an explicit `--session-id` and reaping its
      transcript (`DeleteSessionByIDs`) after each run. Cleaned up the two leftover `/data`
      transcripts.
- [x] 2026-07-11 — **Fix: SSH browse/spawn fails for a folder with no visible subdirs** — the
      visual New-session browser's `ListDir`/`ListAll` probes glob `*/` and `*` over SSH, but the
      remote login shell is zsh, whose NOMATCH aborts the command with exit 1 when the glob matches
      nothing (a directory holding only files and/or dotfiles, e.g. `/data/android`). That surfaced
      as `bad_path: Process exited with status 1` and made such folders un-browsable/un-spawnable.
      Now the probes run under `sh -c` (POSIX leaves an unmatched glob literal, and the `[ -d ]`
      guard skips it). Follow-up: a live-SSH regression test over a files-only temp dir.
- [x] 2026-07-11 — **Headphone-aware hands-free audio + Bluetooth mic toggle** — hands-free no
      longer forces call-mode audio (which ducked other apps, e.g. a movie) when it doesn't need to:
      while headphones are connected (wired/USB/BT), capture runs in plain media mode with no echo
      canceller (TTS is in your ears, nothing to cancel), switching live on plug/unplug via an
      `AudioDeviceCallback`. On the speaker it keeps the comm-audio + AEC barge-in path. New
      **Audio → Hands-free microphone** choice (`micSource` pref): **Phone mic** (default) vs
      **Headset mic**, which forces the Bluetooth hands-free (SCO) profile so a paired headset's mic
      hears you across the room (call quality, ducks other apps — inherent BT trade). Falls back to
      the phone mic when no BT headset is present, and — after `verifyHeadsetMic` gives the SCO link
      ~2.5s and finds it dead (`AudioRouter.headsetMicActive`) — auto-reverts to the built-in mic so
      a headset that won't engage its hands-free profile never leaves the user unheard (latched per
      session, cleared on setting change / fresh start). Client-only, no protocol change.
      `AudioRouter.headphonesConnected/bluetoothMicAvailable/enable|disableHeadsetMic`,
      `HandsFreeRecorder` source+AEC now configurable, `VoiceController.resolveMicProfile`.
- [x] 2026-07-11 — **Gated dictation ("speak token")** — in hands-free mode with ambient chatter
      (other people, radio, recordings), un-bracketed speech is no longer dictated. New per-client
      **dictation gate**: `hello.speak_token` (comma-separated start marker) + `hello.dictation_gate`
      switch. When on, only speech following the speak token (up to the end token) reaches Claude;
      everything else is discarded. Distinct from the "hey buddy" command token — commands are never
      gated, so barge-in ("hey buddy stop") always works. Server: `command.SplitOn` (matches only the
      speak phrases, not the built-in wake) + `conn.gateDictation`, applied at both dictation sinks in
      `commitMessage`; speak token biased into `vocabBias`. Client (commonMain + both backends):
      `Prefs.speakToken`/`dictationGate`, a **Dictation gate** switch + speak-token field in
      `CommandsSettings`, `HelloConfig`/`Outbound.hello`. Unit-tested (`TestSplitOn`,
      `TestGateDictation`); docs in `docs/protocol.md`, `docs/commands.md`, `README.md`. ⚠ needs a
      server restart to go live (client half ships in the APK).
- [x] 2026-07-11 — **Multiple configurable wake words** — `command.WakePhrase` now parses a
      **comma-separated** list, so the app's `wake_token` can hold several misheard variants
      ("hey buddy, hey bud, ok buddy") that all trigger commands (Whisper mis-hears the wake phrase in
      noise). Wire field stays one string; the server splits it. Refactored `wakeAt` to share a
      `phrasesAt` primitive. `CommandsSettings` label + help updated to reflect the list.
      Unit-tested (`TestWakePhrase`, `TestWakePhraseMultiVariant`); docs in `docs/commands.md`,
      `README.md`. ⚠ needs a server restart to go live.

- [x] 2026-07-11 — **Hands-free "transcribing…" state** — the commit path re-transcribes the
      whole buffered clip accurately (a ~1-2 s window), during which the app used to snap the
      hands-free pill back to "listening" before "thinking" appeared. The server now emits a
      payload-free `transcribing` message right before that re-transcribe (cleared by the
      `transcript` that follows, or a `pending` reset if nothing was recognized); the app maps it
      to a new `VoiceState.TRANSCRIBING` pill. Server (`stream.go`, `messages.go`), protocol
      (`docs/protocol.md`, `Protocol.kt`), UI (`ChatModels.kt`, `ChatStatus.kt`, both controllers).
- [x] 2026-07-10 — **Chained "hey buddy" commands in one utterance** — when the end token
      misfires and clips keep accumulating, a committed message can hold several wake phrases. The
      commit path now uses a new `command.SplitWakeAll` to split on **every** "hey buddy" and run
      the commands **in order**; if any segment is `cancel` (built-in "cancel" / "cancel that"), the
      whole committed message is scrapped and nothing runs — the voice escape hatch for a runaway
      draft. Server-side only (`stream.go`, `command.go`, registry + `commands.json`); unit-tested
      (`TestSplitWakeAll`); docs in `docs/commands.md`.
- [x] 2026-07-10 — **Debug overlays (Settings → Debug)** — diagnose the fiddly hold-to-talk. New
      `debugOverlays` pref + Debug settings page; when on, `InputBar` draws the push-to-talk
      cancel (drag-left) and hands-free (drag-up) zones as translucent boxes with a live drift
      readout, and logs each hold's end reason + drift to logcat (tag `PTT`). Instrumented the
      gesture to record *why* a hold ended, including a `lost-pointer` case (OS dropped our pointer
      id mid-hold) — the prime suspect for spurious cuts. Off by default; Android + web compile.
- [x] 2026-07-11 — **Fix: spurious hold-to-talk cut = system edge gestures stealing the touch.**
      Cause confirmed as `lost-pointer`: when the thumb drifts toward the right screen edge (the
      back-swipe zone) or down into the navigation-bar/home zone, Android's own gestures claim the
      in-progress touch and deliver our mic button a CANCEL — the pointer id vanishes, the gesture
      loop breaks, and `talking` still true → `onTalkStop()` commits the truncated clip mid-sentence.
      Fixed by reserving the mic button's rect (grown down into the nav-bar zone + left along the
      cancel track) from the platform gestures via a new `Modifier.pttGestureExclusion` seam
      (Android `systemGestureExclusion`; web no-op), active only while the button is a live mic.
      `PttExclusion.kt` (+ android/wasmJs actuals), applied in `InputBar`.
- [x] 2026-07-10 — **Per-server whisper model pair**: Settings → Audio → "Transcription models" —
      two free-text ggml model fields, **full** (accurate server, dictation) and **quick** (fast
      server: hands-free draft + end-token detection), each hot-loaded server-globally.
      `set_whisper_model` gained a `fast` flag; `hello_ok`/`whisper_model` now report both models;
      the fast choice persists in `settings.json` and boots via `SPAWNER_WHISPER_FAST_MODEL_NAME`.
      Replaces the fixed three-pill picker in Settings → Server. ⚠ needs a server restart to go
      live.
- [x] 2026-07-11 — **Summary-only speech** (`hey buddy summary only` / `speak everything`): on a
      long multi-step turn, speak only the final result and play a soft warm beep in place of reading
      each intermediate streamed step aloud (everything still shown on screen). New `summary_only`
      command + `speech_mode` wire message relay the toggle to the client; the state lives client-side
      (persisted `summary_only_speech` pref, mirrored by the **Summary only** switch on the Audio
      settings page) so the server keeps none. Gating keys off the existing `chunk` flag (intermediate
      streamed step vs final result). Beep is a synthesized low sine w/ raised-cosine envelope
      (`Speaker.beep` on Android via AudioTrack; `webBeep` on web via WebAudio), routed through the
      echo-cancelled voice path in hands-free. Registry + parse + vocab + protocol.md + commands.md +
      README + tests. ⚠ needs a server restart to go live (client half ships in the APK).
- [x] 2026-07-10 — **Scratch mode** (`hey buddy scratch on/off`): new `scratch` command toggles a
      per-connection flag; while detached, `dispatch`/`commitMessage` echo each non-command
      transcription back via `say` (reusing the existing wire) so you can test STT quality. Registry
      + parse + vocab + docs (commands.md, README). ⚠ needs a server restart to go live.
- [x] 2026-07-10 — **Model picker dropdown**: new `SPAWNER_WHISPER_MODELS_DIR` lets the gateway
      list the ggml models on disk (size-ordered, re-scanned per send); `hello_ok`/`whisper_model`
      carry them as `whisper_models`, and the app's Transcription models fields become dropdowns
      when the list is non-empty (free text stays the fallback). Apply also works on an unchanged
      name to pin an env-default model into `settings.json`. ⚠ needs a container rebuild (new env
      var + binary) to go live.

### Multi-backend AI registry + per-session model selection (epic — proposed 2026-07-09)

Generalize the server from "drives `claude` only" to a registry of headless AI backends, each
declaring its binary, how it builds a turn's command line, how its output is parsed, a **default
model**, and its list of **selectable models** (by spoken alias — opus/sonnet/fable). A session
records which backend + model it uses; spawn stamps the backend's default model; voice can override
the model later. Backend (which AI) is orthogonal to Executor (where it runs — host/sandbox/SSH), so
any backend runs on any target.

- [x] `internal/agent` registry package: `Agent` (id, name, output `Format`, `DefaultModel`,
      `Models`, per-backend arg builder), `Model` (alias/flag/spoken), `Registry`
      (`Get`/`Resolve`/`Default`/`List`), and the `claude` entry whose `Args` reproduce the legacy
      Turn command line plus `--model`. Unit tests cover resolution + arg building. (2026-07-09)
- [x] Wire the registry into `session`: `Session.Agent` + `Session.Model` fields (persisted in
      `sessions.json`, empty = default backend/model for old records); `Driver.Turn` builds args via
      the session's `Agent.Args(TurnSpec{...})` instead of the hardcoded slice; parser dispatch on
      `Agent.Format` (nil registry lazily defaults, so Driver literals still resolve). (2026-07-09)
- [x] Per-backend binaries at the Executor seam: `Executor.Start` takes a `bin` param the Driver
      resolves from the session's agent (`Driver.binFor` / `AgentBins`); empty defers to each
      executor's own `SPAWNER_*_CLAUDE_BIN`, so Claude is untouched. Host binary override via
      `SPAWNER_CODEX_BIN`; per-target Codex bins via `SPAWNER_SANDBOX_CODEX_BIN` /
      `SPAWNER_SSH_CODEX_BIN` (`AgentBins` is now `map[agent]map[target]bin`; SSH reuses the host
      target, so its bin feeds the host entry when `SPAWNER_SSH` is on). (2026-07-09)
- [x] Register Codex as the second backend: `agent.codex()` (id `codex`, bin `codex`, format
      `FormatCodexJSONL`, `SelfAssignsID`, default model `gpt-5.5` + reasoning-effort presets).
      `codex exec`/`codex exec resume`; Codex mints its own `thread_id`, captured from the stream by
      `parseCodexStream` and adopted as the session id. Arg/parse shapes verified against a live
      `codex-cli` 0.144.1. Unit tests for args + parser; command lines validated end-to-end. Note:
      on a ChatGPT-account plan only `gpt-5.5` is `-m`-selectable, so the alternates are
      reasoning-effort presets on it. (2026-07-09)
- [x] Codex sessions replay their transcript on reattach (like Claude): `history` and the on-attach
      context badge were Claude-only (`claudeFS` reads `~/.claude/projects/*/<id>.jsonl`), so Codex
      rows came back empty. Added `codexFS` (`internal/session/codex_transcript.go`) reading Codex's
      rollout JSONL (`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl`) — prose from
      `event_msg` `user_message`/`agent_message`, context from `token_count`; `Driver.transcriptReaderFor`
      picks the reader by `Agent.Format`, and `ReadTranscriptChain`/`LastContextUsage` now take the
      agent id (gateway `serveHistory`/`doAttach`/turn badge pass `s.Agent`). Unit tests +
      architecture doc. (2026-07-09)
- [x] Spawn stamps agent + `DefaultModel`: `newSession` (the single choke point for both spawn
      paths) sets `Session.Agent` and `Session.Model = agent.DefaultModel`. Inline **voice backend
      selection**: "spawn a codex session" / "spawn a session on codex" → `command.extractSpawnAgent`
      pulls the backend out (only in selector position, so a path token like `.../codex-x` isn't
      mistaken; "claude" deliberately not a keyword — it's the default and a common path word),
      threaded through the spawn dialog to `newSession`. Parse tests. (2026-07-09)
- [x] Surface agent/model over the protocol (server half): `agent`/`model` on `session_list`,
      `attached`, and `discovered` (registered rows), omitted for the default Claude / pre-registry
      records. `docs/protocol.md` updated (drift test checks types only, so field docs are
      discipline). (2026-07-09)
- [x] App-facing new-session picker: the `agents` outbound message advertises the backend registry
      on connect; the app parses it (`AgentInfo`, `ServerMsg.Agents`, `AppController.agents`) and the
      BrowseScreen shows a backend chip row (when >1 backend) + a model chip row, sent in `spawn_at`
      (`agent`/`model`). Server validates the model against the chosen backend. Both controllers
      (Android + web) updated for no-divergence. (2026-07-09)
- [x] Remaining app polish: session rows show a backend/model badge (shared `backendBadge` helper in
      `Sidebar.kt`, dropping the backend prefix for the default Claude), the `TopBar` shows the
      attached session's backend/model badge (`attachedAgent`/`attachedModel` flows on `AppController`,
      fed from the `attached` message), and the new-session `BrowseScreen` — with its backend/model/host
      pickers — moved to `commonMain` typed against `AppController`, so the **web** client now has the
      full spawn flow too (`WebRoot` routes `onNewSession` → `browse`). (2026-07-09)
- [ ] Optional: `agent`/`model` on the `spawn_at` inbound message so the visual picker can spawn a
      chosen backend (voice spawn selection already works).
- [x] Voice model selection: "hey buddy, list models" speaks the attached session's backend
      catalogue numbered (marking current); "hey buddy, use model 3" switches by ordinal (digit or
      number-word) — ordinals dodge hard-to-say model names. `internal/command` (ListModels/UseModel
      intents + `modelIndex`), gateway `doListModels`/`doUseModel`, `docs/commands.md` + regenerated
      `commands.json`, parse tests. Durable on the session; applies next turn. (2026-07-09)
- [ ] Register a second real backend (TBD which) — the proof the seam holds: a new `Agent` entry
      (+ parser if its output isn't stream-json), no changes to the session/executor/gateway core.
- [ ] Docs: `README.md` (user-facing: choosing a backend + model), `docs/architecture.md` (the
      backend-vs-executor seam), `CLAUDE.md` config section for any new env vars.

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

  - [x] 2026-07-09 — **(a) Widen the shared controller interface.** New `commonMain/AppController.kt`
        defines `AppController : HostsIdentitiesController`, exposing the whole shared UI surface as
        `StateFlow`s (`chat`, `status`, `attachedName`, `activity`, `pending`, `voiceState`, `speaking`,
        `lastTurnUsage`/`rateLimit`/`usageEstimate`/`usageReport`, `discovered`, `hasMoreHistory`,
        `scrollTick`, `listing`, `fileSaved`/`fileData`, `ask`, `whisperModel`, …) + methods (`sendText`,
        `attachTo`/`detach`, `abortTurn`, `loadOlder`, `discover`/`adopt`/`rename`/`delete`, `spawnAt`/
        `spawnNewFolder`, `browse`/`upload`/`download`, usage + server controls). `TurnUsageInfo` moved to
        `commonMain`. `VoiceController` now `: AppController` with members marked `override`; audio/mic/TTS/
        connect/lifecycle stay off the interface (driven by the concrete class). Both targets build.
  - [x] 2026-07-09 — **(b) Prefs abstraction.** New `commonMain/Prefs.kt` interface with typed
        get/set for every shared key (url/token/clientId, theme/badge/cache-warm, hands-free/audio/
        brief/interactive/end-token, STT/whisper, VAD, command-aliases). Alias parsing (`aliasMap`/
        `addAlias`/`removeAlias`) is shared as interface default methods; the `DEFAULT_*` constants
        moved to `Prefs.companion` so both clients use identical defaults. `SettingsStore` now
        `: Prefs` (props `override`, dup alias logic dropped); client-cert file I/O stays Android-only.
        A web `localStorage`-backed `Prefs` comes with the web controller (step e / M5). Both build.
  - [ ] **(c) Then lift, screen by screen (each its own commit, both targets green):**
        - [x] 2026-07-09 — `SettingsHub` + `SettingsRow` (pure), `AppearanceSettings` (theme/badge/
              cache-warm), and the shared `ThemeChoice` pill lifted into `commonMain/SettingsScreens.kt`;
              `AppearanceSettings` retyped against `Prefs`. `ThemeMode`/`parseThemeMode` moved to
              `commonMain/ui/ThemeMode.kt` (Android `SpawnerTheme` keeps its status-bar side effect).
              Both targets build.
        - [x] 2026-07-09 — `CommandsSettings` + its closure (`CommandAliasGroup`, `AliasChip`,
              `AddAliasForCommandDialog`) lifted into `commonMain/SettingsScreens.kt`, retyped against
              `Prefs`; uses the shared `COMMANDS`/`Command` and the `Prefs` alias helpers. Both build.
        - [x] 2026-07-09 — `ServerSettings` lifted into `commonMain/SettingsScreens.kt` (URL/token +
              Save & Connect, whisper-model picker, restart), retyped against `Prefs` + `AppController`.
              The mutual-TLS `.p12` importer is a `certSection: @Composable ColumnScope.() -> Unit`
              slot — Android fills it with `ServerCertSection` (SAF `OpenDocument` picker +
              client-cert prefs, still in `MainActivity`); web leaves it empty. Both build.
        - [x] 2026-07-09 — `AudioSettings` lifted into `commonMain/SettingsScreens.kt` (threshold +
              VAD sliders, brief/interactive toggles, end token, silence auto-commit, whisper URL),
              retyped against `Prefs`; `VadSlider` moved to common too. The two audio-hardware pieces
              are platform slots: `micMeter` (Android draws `LevelMeterBar` off `micLevel` +
              start/stop the meter; web empty) and `endTokenTest` (Android's calibration Test button +
              `CalibrationDialog`; web empty). `LevelMeterBar`/`CalibrationDialog` stay in `MainActivity`.
              **All (c) settings screens now shared.** Both build.
        - [x] 2026-07-09 — `TopBar` + `AudioOutputButton` + `CacheWarmBar` lifted into
              `commonMain/TopBar.kt`. `AudioOutput` enum moved to `commonMain/audio/AudioOutput.kt`
              (the `AudioRouter` hardware stays Android-only, same package). `CacheWarmBar` now uses
              the shared `nowMonotonicMs()` clock seam and manual mm:ss formatting (no `String.format`).
              TopBar takes the output as params, so no `AppController` change was needed. Both build.
        - [x] 2026-07-09 — Usage UI (`UsageSheet`, `UsageBar`, `UsageEstimateLine`, `SessionLimitFooter`)
              + helpers (`pctStr`, `fmtTokL` with `String.format` rewritten, `relativeTime`,
              `relResetSuffix`) lifted into `commonMain/Usage.kt`; added `nowEpochSeconds()`/`fmtClock()`
              seams so the rate-limit footer drops its `Context` dependency.
        - [x] 2026-07-09 — `Sidebar` (sessions grouped by host, pull-to-refresh, detach, usage footer)
              lifted into `commonMain/Sidebar.kt`; `LOCAL_HOST` const moved to common. Fully
              parameterized over shared types; `PullToRefreshBox`/`LazyColumn` compile in common. Both build.
        - [x] 2026-07-09 — `InputBar` + `CommandTray` lifted into `commonMain/InputBar.kt`. The whole
              WhatsApp-style send/mic/hands-free gesture is pure Compose driven through the existing
              `onTalkStart`/`onTalkStop`/`onTalkCancel`/`onToggleHandsFree`/`onSend` callbacks, so the
              concrete controller no longer appears. The 📎 transfer button is a `transferButton`
              slot (Android fills it with `TransferButton`'s SAF/Base64 flow; web empty until M5).
              Both build.
        - [x] 2026-07-09 — `MainScreen` lifted into `commonMain/MainScreen.kt` (drawer + `Sidebar`,
              `TopBar`, `ChatList`, status bars, `InputBar`, and the hoisted session/usage dialogs),
              retyped against `AppController`. Added a `PlatformBackHandler` expect/actual seam
              (Android `BackHandler`; web no-op). The audio-hardware surface stays off `AppController`
              and is passed in: `mic`/`audioOutput`/`audioOutputs` values + `onSelectAudioOutput`/
              `onRefreshOutputs`/`onTalkStart`/`onTalkStop`/`onTalkCancel`/`onStopSpeaking` callbacks,
              and the 📎 `transferButton` slot. `AppRoot` stays Android (it routes to the Android-only
              `BrowseScreen`/`ServerCertSection` and owns permissions/lifecycle) and hosts the shared
              `MainScreen`. **Verified: `:app:assembleDebug` (APK) + `:app:wasmJsBrowserDistribution`
              (web bundle) both build.**
  - [x] **(d) Remaining platform seams — done.** (2026-07-11) Done: prefs (`Prefs`/`WebPrefs`),
        monotonic + wall clock (`Clock.kt`), file pickers (InputBar 📎 slot), back handler
        (`PlatformBackHandler`), `AudioOutput`/`ThemeMode` shared, and now **status-bar chrome**:
        `SpawnerTheme` moved to `commonMain/ui/Theme.kt` over a new `expect fun
        ApplySystemBarAppearance(dark)` — Android's `actual` tints the window insets controller,
        web's is a no-op. `WebRoot` uses the shared `SpawnerTheme` instead of inline `MaterialTheme`.
        Audio capture / wake word / TTS / notifications / foreground service are M5 (web gets Web
        Audio + `SpeechSynthesis`); until then the web controller stubs them.
  - [x] 2026-07-09 — **(e) Web controller + entry point wired.** `wasmJsMain/WebPrefs.kt`
        (`localStorage`-backed `Prefs`), `WebAppController.kt` (implements `AppController`, wiring a real
        shared `SpawnerClient`'s `ServerMsg`s → state flows and methods → `Outbound` sends; replicates the
        non-audio message handling — chat/history, attach, discovery, hosts/identities, usage, ask, file
        transfer), and `WebRoot.kt` (navigation shell over the shared screens, auto-connects on load,
        stubs the audio params). `main.kt` now mounts `WebRoot`. **Verified end-to-end: the
        `:app:wasmJsBrowserDistribution` bundle, served locally and loaded in Firefox (Kotlin/Wasm),
        rendered the shared UI and completed a live WebSocket connect + hello handshake against the
        running server — the top bar showed "Claude Spawner · connected" with the detached banner, chat,
        and input bar all drawing from the shared composables. M2's deferred live-connect check is now done.**
- [x] **M4 — Responsive layout + desktop affordances.** (2026-07-10)
  - [x] 2026-07-10 — **Responsive sidebar.** `MainScreen` now branches on window width via
        `BoxWithConstraints`: a narrow window (phone, <840.dp) keeps the swipe-in `ModalNavigationDrawer`;
        a wide one (desktop browser, tablet, unfolded, ≥840.dp) pins the sessions `Sidebar` permanently
        beside the chat with `PermanentNavigationDrawer` (320.dp sheet) and drops the ☰ toggle
        (`TopBar.onMenu` is now nullable). The `Sidebar` and chat column were extracted into two local
        composables shared by both branches — same composables, different container. Both targets build.
  - [x] 2026-07-09 — **Discoverable controls for the touch gestures.** Mouse/desktop users had no
        obvious way to trigger the swipe gestures, so each got a visible affordance (all shared
        commonMain, so they appear on Android too — harmless, the gestures still work): a chevron
        handle above the message box toggles the command tray (the swipe-up); a **Refresh** button
        beside **New** in the sessions drawer (the pull-to-refresh); and **Enter sends / Shift+Enter
        newlines** on the web client (gated by `platformName() == "Web"`, so mobile keeps Enter as a
        newline). Both targets compile green.
  - [x] 2026-07-11 — **Composer expands to full width when multi-line.** Once the draft grows past
        one line, `InputBar` moves the text field to its own full-width row above the transfer/send
        buttons (they drop to a row beneath) instead of leaving it a skinny column wedged between
        them — the common phone-messenger behaviour. Height-measured via `onSizeChanged` against the
        empty single-line baseline; stays expanded until the box is cleared so it can't oscillate at
        the boundary. Built as one custom `Layout` that re-arranges the transfer/field/send
        children in the measure pass (rather than swapping a Row for a Column), so the field's
        node is never re-parented and keeps its focus + the soft keyboard across the
        expand↔collapse transition. Verified on the emulator (both layouts, keyboard stays up)
        and installed on the phone.
  - [x] 2026-07-11 — **Composer border fills its slot instead of creeping out with the text.** The
        `Layout` measured the field slot edge-to-edge, but the wrapping `Box` loosened `minWidth` to
        0, so the `OutlinedTextField` sized to its text content and the purple border only grew to
        the edge as you typed. A `Modifier.fillMaxWidth()` on the field pins the border to the full
        slot width in both the collapsed and expanded layouts. Built clean and installed on the phone.
  - [x] 2026-07-11 — **Chat bubbles cap at 80% of the window width, not a fixed 320dp.** The old
        hard cap looked right on a phone but left a skinny column on a tablet; a `BoxWithConstraints`
        now sets `widthIn(max = maxWidth * 0.8f)` so bubbles grow with the window while short
        messages still hug their text. Built clean and installed on the phone.
  - [x] 2026-07-11 — **Shift+Enter sends from any physical keyboard.** Replaces the web-only
        "plain Enter sends"; now Shift+Enter sends and plain Enter is a newline on both the web
        client and a Bluetooth keyboard paired to the Android app (on-screen keyboards never emit
        the chord, so touch typing is unaffected). Built clean and installed on the phone.
- [~] **M5 — Web-native platform bits.** Browser audio (Web Audio → server STT), `SpeechSynthesis` TTS,
      browser spawn UI.
  - [x] 2026-07-09 — **Web file transfer (the 📎 flow).** The web `MainScreen` now fills the
        `transferButton` slot with a browser upload/download button over the existing WebSocket
        `upload`/`download` protocol (already implemented in `WebAppController`): the DOM File API
        reads a picked file → base64 → `uploadFile`; a downloaded `file_data` is saved via a blob
        object-URL. The host directory/file picker (`TransferPickerDialog`) was promoted to commonMain
        (typed against `AppController`, glyphs → Material icons) so Android and web share one picker.
  - [x] 2026-07-09 — **Browser spawn UI.** The Android-only `BrowseScreen` moved to
        `commonMain/BrowseScreen.kt`, retyped against `AppController` (only `collectAsStateWithLifecycle`
        → `collectAsState` differed; every data path — `listing`/`hosts`/`agents`/`browse`/`spawnAt`/
        `spawnNewFolder`/`requestHosts` — was already on the interface). `WebRoot` routes
        `onNewSession` → a `"browse"` screen, so the web client gets the full New-session flow
        (target/host + backend/model chips + filesystem browse). Both targets compile green.
  - [x] 2026-07-10 — **Browser voice: push-to-talk mic + SpeechSynthesis TTS.** New
        `wasmJsMain/WebAudio.kt` (`js(...)` helpers): `startMic`/`stopMic`/`cancelMic` capture the mic
        via Web Audio (getUserMedia → ScriptProcessor), accumulate Float32, and on release downsample
        to 16 kHz mono PCM16LE returned as base64; `speakText`/`cancelSpeech`/`speechActive` drive the
        browser's `SpeechSynthesis`. `WebAppController` gained `startTalking`/`stopTalking`/
        `cancelTalking` (send `wake("pcm16")` → `sendAudio(clip)` → `audio_end`, reusing the phone's
        push-to-talk wire path — no Opus/ffmpeg) + a `micText` flow, and now speaks `say`/`output`
        (markdown-stripped via the shared `Markdown`, moved to `commonMain`), cancels on `stop_speaking`
        and barges-in on talk-start; a poll flips `speaking` off when the utterance queue drains.
        `WebRoot` wires the four talk/stop callbacks + `mic` text. Needs a secure context + mic
        permission. Both targets build; wasm bundle packages. **Still browser-TODO below.**
  - [x] 2026-07-11 — **Hands-free (VAD-gated always-listening) in the browser.** New
        `WebAudio.startHandsFreeMic/pollHandsFreeClip/handsFreeCapturing/stopHandsFreeMic`: one open
        mic (getUserMedia + ScriptProcessor) with a JS energy VAD that mirrors the phone's
        `Endpointer` — RMS scaled to int16 so the shared `Prefs.vadThreshold`/`vadOnsetMs`/
        `vadSilenceMs` mean the same thing; onset starts an utterance (keeping a pre-roll so the
        first word isn't clipped), silence (or the 15 s cap) ends it. Each finished clip is
        downsampled to 16 kHz pcm16 and queued on a `window` global; `WebAppController` drains it in
        a poll loop and ships it via `wake(pcm16, hands_free=true)` → audio → `audio_end` (same wire
        as the phone; server streaming-appends until the end token). Echo rejected by tripling the
        VAD bar while `speechSynthesis.speaking` (plus getUserMedia's own echoCancellation).
        `voiceState` now tracks LISTENING/CAPTURING/SPEAKING. Per-session toggle (not auto-started —
        getUserMedia needs a user gesture). ⚠ live browser test pending.
  - [x] 2026-07-11 — **Browser audio-output control (Speaker/Mute).** Browsers speak via
        `SpeechSynthesis` to the OS default sink and expose no routing, so the picker now offers the
        two states that matter: Speaker (voice on) vs Mute (voice off). `WebAppController` owns an
        `audioOutput` flow + `setAudioOutput`, persists it via `Prefs.audioOutput`, gates `speak()`
        on it, and cancels any in-flight utterance when muted; `WebRoot` feeds it into the shared
        `TopBar` output button. (Any saved earpiece/bluetooth value from the phone normalizes to
        Speaker.) Replaces the old MUTE-only stub.
  - [x] `localStorage`-backed prefs — done earlier with `WebPrefs`.
- [~] **M6 — Serve + document.** (in progress)
  - [x] 2026-07-09 — **Server hosts the web bundle.** New `SPAWNER_WEB_DIR` config: when set, the Go
        server serves that directory (the built Compose/Wasm bundle) as static files at `/` alongside
        the `/ws` gateway, so one binary hosts both. `/ws` + `/healthz` keep precedence; static assets
        are public, the privileged surface stays behind the token-authed `/ws`. Go's `FileServer`
        serves `.wasm` as `application/wasm`. The web bundle defaults its WebSocket to the **same
        origin** (`/ws`, `wss://` on https) so a server-hosted client connects with no setup.
        Documented in `CLAUDE.md` (config) + `README.md` (web-client build/run section). Verified on a
        scratch port: `/`→index.html, `/spawnerweb.wasm`→`application/wasm`, `/healthz`→ok; `go build`
        + `go test ./...` green. (Live browser load over the server itself was verified next; the
        status-bar `expect/actual` seam landed 2026-07-11 — see M3 step (d).)
  - [x] 2026-07-09 — **Live browser verified + two connect blockers fixed.** The web client is served
        by the container (`:8098`) and reachable at `https://claude.bam` (Caddy `tls <cert> <key>` with
        a self-signed cert in `deploy/tls/`, set via caddyedit — the host Caddy's internal CA had an
        expired intermediate). Fixed: (1) `crypto.randomUUID` is undefined in an **insecure context**
        (plain http on a real host), which threw during connect → `WebPrefs` now falls back; (2) the
        client works only over a **secure context** (https/localhost) — over plain http the prod
        (wasm-opt) bundle can't connect, so serve over https. Firefox Mobile blanks on the huge dev
        bundle, so the **production** bundle is served.
  - [x] 2026-07-09 — **Shared UI uses Material vector icons, not emoji.** Every emoji/symbol glyph in
        the common composables (menu, settings, mic, send, back, edit, delete, usage, cache warm/cold,
        audio outputs, …) is now an `Icons.Filled.*` / `Icons.AutoMirrored.Filled.*` vector via
        `compose.materialIconsExtended`. Emoji rendered as blank tofu boxes in the browser (Skiko has no
        system emoji font); vectors render on every target. Both `compileDebugKotlinAndroid` and
        `compileKotlinWasmJs` green; icons verified rendering live at `https://claude.bam`.

### File upload/download over the WebSocket (proposed 2026-07-08)

A 📎 button left of the message box transfers files between the phone and the session's host, over the
same authenticated socket (base64 in one message each way, 64 MiB cap).

- [x] 2026-07-08 — **Server half.** New `upload` (write a base64 file to `<dir>/<name>` on `host_name`)
      and `download` (read a file, return `file_data` base64) messages; `browse` gained a `files` flag so
      the picker can also list regular files (`listing` entries now carry a `dir` flag, directories first).
      `SSHPool.ReadFile/WriteFile/ListAll` do the host-side I/O over the pooled SSH connection (loopback
      for local); local-FS fallback when SSH is disabled. Docs: `docs/protocol.md` (`upload`, `download`,
      `file_saved`, `file_data`, `file_too_large`), README. Errors: `file_too_large`, `bad_path`.
- [x] 2026-07-11 — **Android half.** 📎 button left of the input bar → upload/download menu. Upload:
      SAF `OpenDocument` picks a local file → the shared `TransferPickerDialog` (host-scoped browser)
      picks a destination folder → `controller.uploadFile`; on `file_saved`, the draft prefills with
      `look at the file at <path>`. Download: files-mode `TransferPickerDialog` picks a file →
      `controller.downloadFile`; on `file_data`, SAF `CreateDocument` saves it. Fully wired in
      `MainActivity.kt` (`TransferButton`) + `VoiceController.kt`; nothing stubbed.

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

## Done

- [x] 2026-07-10 — **Offline transcript cache + digest-guarded history.** The app now persists each
      session's chat log to disk (`TranscriptCache`, one JSON file per session) so history is available
      offline and switching sessions doesn't re-download seen messages. New `digest` → `digests` wire
      messages carry a per-session message count + content hash (`session.HistoryDigest`, sha256) with
      no bodies; the app requests them on connect and compares against its cached digest. `history`
      gained an optional `have_hash` (server replies `unchanged: true` with no bodies when it still
      matches) and now returns the chain's `count`/`hash` on every page. An (re)attach skips the fetch
      entirely when the cached digest equals the server's; a live user/claude line invalidates the
      server digest so the next reattach refetches; a `clear`/`compress` rewrite changes the hash and
      triggers a full refetch. Server (`serveDigests`, hash-guarded `serveHistory`) + Android (cache,
      `ensureLoaded`/`persist`, attach/onHistory/HelloOk/Digests wiring) + `HistoryDigest` unit tests;
      docs in `protocol.md` + `README.md`. Verified: server `go test`, APK build + emulator install;
      live end-to-end pending the server deploy.
- [x] 2026-07-10 — **Sidebar sessions as cards + details/edit dialog with agent switching.** Each
      session in the drawer is now a card showing name, AI backend/model, and a sandbox badge; tapping
      it opens a details sheet (path + Open/Edit/Delete). **Edit** renames and can switch the session's
      AI agent + model via a new `set_agent` wire message — changing the backend rotates to a fresh
      `session_id` and restarts the conversation (Claude/Codex transcripts are incompatible on disk),
      while changing only the model is preserved. Client (Sidebar/MainScreen + `Outbound.setAgent` +
      `AppController.setAgent` in all three source sets) and server (`doSetAgent`) both done; docs in
      `protocol.md` + `README.md`.

- [x] 2026-07-10 — **Two compression triggers (warm + auto), moved to Server settings.** The single
      warm-cache-window auto-compress became **warm compress** (opportunistic, fires in the last ~15 s
      of the warm window); added a new **auto compress** that fires the moment an idle session crosses
      the shared token limit (immediate, no warm-window wait; wins if both on). Wire: `auto_compress`
      message + hello now carry `warm_compress` + `auto_compress` + shared `auto_compress_threshold`.
      Moved the whole UI off the Appearance page onto **Settings → Server**. Also reweighted the
      compress prompt to keep the most recent exchanges near-verbatim and squeeze older history harder.
      Note: the old single `auto_compress` client pref is orphaned (both toggles default off) — safe
      default; reconfigure on the Server page. Docs updated (protocol.md/README).

- [x] 2026-07-10 — **Settable custom wake token + regrouped Commands settings.** The wake word was
      hardcoded server-side (`command.wakePhrases`). Added an optional `wake_token` to the `hello`
      handshake: the app's Commands settings page can now set a custom wake word, accepted *alongside*
      the built-in "hey buddy" (blank = built-in only). Server folds it in via `command.WakePhrase` →
      `StripWakeWith`/`SplitWakeWith` (new `extra`-phrase variants of the wake matchers) and biases
      Whisper toward it in `vocabBias`. Also **moved the end token off the Audio page onto the Commands
      page** (both tokens are command grammar) and relabeled the remaining "Silence auto-commit" field.
      The Audio "Silence to end" VAD slider stays — it's the live hands-free end-of-utterance timer.
      Client pref added to all three source sets; command-package unit tests cover the custom-token
      matching; docs updated (commands.md/protocol.md/README). Silence auto-commit was moved to the
      Commands page too (it's client-local, no reconnect), leaving Audio to pure VAD/TTS/whisper dials.
- [x] 2026-07-10 — **Persist server-global settings across restart (`settings.json`).** The
      hot-swappable resident whisper model was held only in memory, so a restart/rebuild reverted it to
      `SPAWNER_WHISPER_MODEL_NAME`. New `internal/session/settings.go` (`SettingsStore`, mirrors the
      `Store` atomic-write pattern) persists it to `settings.json` next to the session state; the
      gateway seeds its boot model from the file (persisted choice wins over the env default) and
      `doSetWhisperModel` writes the file on every change. Unit tests cover round-trip + empty-path
      in-memory mode. Docs: README (whisper section), `docs/architecture.md` (layout).

- [x] 2026-07-10 — **Remove the in-app mutual-TLS client-certificate importer.** TLS is now terminated
      at the reverse proxy (Caddy), so the app no longer imports a `.p12`/presents a client cert. Dropped
      the `certSection` slot from `ServerSettings`, `ServerCertSection` + the SAF picker, the
      `clientCert*` prefs/methods in `SettingsStore`, the `ClientTls` expect/actual + `buildClientTls`,
      and the `tls` param threaded through `SpawnerClient`/`spawnerHttpClient` (renamed
      `ClientTls.*.kt` → `HttpTransport.*.kt`). Server-side `SPAWNER_TLS_CLIENT_CA` stays for
      non-app/proxy-enforced mTLS. Docs: README (transport TLS section).

- [x] 2026-07-10 — **Auto-compress near the warm-cache edge.** New Appearance toggle + token-limit
      (thousands) setting; the app sends the global preference in `hello` and live via a new
      `auto_compress` message. A server-owned monitor (`internal/gateway/autocompress.go`) scans every
      started session each 5 s and fires `startCompress` once its context (input+cache) exceeds the
      limit and it's within ~15 s of its 5-minute warm-cache window expiring — so the summary turn
      reuses the still-warm cache instead of a cold rebuild. Fires even when detached. Docs: README
      (clear-vs-compress), `docs/protocol.md` (`auto_compress` + hello fields). Both app targets build;
      **live verification on the phone still pending** (needs a session to actually cross the threshold
      near cache expiry).

- [x] 2026-07-09 — **Sandbox sessions on the containerized (SSH-native) server.** A `target: sandbox`
      session (e.g. `email`) failed with `has no host set` on the containerized server: it has no
      container runtime, so the sandbox target wasn't registered and the turn fell back to the SSH
      host executor, which rejects a hostless session. Fix: made `SandboxExecutor` SSH-aware — with a
      `Pool`/`Host` set it drives rootless podman **on the host over SSH** (lifecycle via
      `SSHPool.Run`, the exec turn via the new shared `SSHPool.Stream`/`streamRemote` streaming
      helper, `shellJoinCmd` quoting), keeping local child-process exec when `Pool` is nil. Wired in
      `main.go` (SSH on + sandbox image → pool + `localhost`); enabled `SPAWNER_SANDBOX_*` in
      `deploy/spawner-container.env(.example)`. Tests: `TestSandboxSSHInnerCommandQuoted`,
      `TestSandboxHostDefault`. Takes effect on the next container rebuild (`compose up -d --build`).
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

# Config env vars

The authoritative reference for every `SPAWNER_*` environment variable. All are read in
`internal/config`; the `internal/docsync` drift test requires each to appear **here**, backticked,
so a new var added to the code without a note here fails the build. (`CLAUDE.md`'s config section is
just a pointer to this file.)

- `SPAWNER_ADDR` (`:8080`), `SPAWNER_TOKEN` (**required**), `SPAWNER_WEB_DIR` (empty = disabled; a
  directory holding the built Compose/Wasm web-client bundle — `index.html` + `spawnerweb.js` +
  `.wasm` — served as static files at `/` alongside the `/ws` gateway, so one binary hosts both the
  API and the browser client. The static assets are public; the sensitive surface stays behind the
  token-authenticated `/ws` handshake. In the containerized deploy the bundle is **baked into the
  image** at `/srv/web` — `deploy/rebuild-container.sh` stages the Gradle output into the build
  context (building it in a throwaway Gradle container if missing, so a fresh clone's first deploy
  ships the client too), so a `rebuild` ships the current client with no host mount),
  `SPAWNER_STATE` (`sessions.json`), `SPAWNER_PROFILES` (`profiles.json`; optional
  app-managed JSON execution-profile catalogue — the app is the source of truth (like
  `hosts.json`/`identities.json`), the server persists it and re-broadcasts on change. A missing file
  is seeded on first run with starter profiles from the flat sandbox env vars below — `bare-metal`
  (host, marked default), plus `sandbox`/`locked` when `SPAWNER_SANDBOX_IMAGE` is set — then the app
  owns it. "Default" is a per-profile marker the user sets, not a fixed profile),
  `SPAWNER_PROFILE_VARS` (optional JSON object of string values — the server-wide `{{.Vars.X}}`
  substitution set for profile templating, e.g. `{"OllamaHost":"pickle.bam.net"}`. A profile's own
  `vars` overlay these; profile-derived built-ins are `{{.Home}}`, `{{.Session}}`, `{{.Dir}}`.
  Referencing an undefined var fails the turn loudly. Empty = no global vars),
  `SPAWNER_PROVIDERS` (`providers.json`; optional app-managed JSON overlay of
  per-backend (AI-provider) settings — the model a fresh spawn defaults to and which
  models the voice `list models`/`use model N` commands enumerate (Settings →
  Providers). The backends themselves are compile-time; this only stores the user's
  overrides, validated against the live registry. A missing file means no overrides
  (compiled default model, all models voice-enabled). Like the profile/host
  catalogues, the app is the source of truth and the server persists it and
  re-broadcasts the `agents` message on change),
  `SPAWNER_SPOKEN_TOKENS` (`spoken_tokens.json`; the app-managed spoken-token catalogue
  — the wake/end/speak phrases and their optional dedicated-detector (ONNX) models.
  Each token binds a spoken phrase to one of a closed, code-defined set of actions
  (wake, end, speech-gate — advertised to the app as the `actions` message); several
  tokens may share an action (so "hey buddy" and "hey gecko" both wake). A token with
  a model is scored by that model when the wakeword sidecar is on, else it falls back
  to Whisper string-matching. Like the profile/host catalogues, the app is the source
  of truth and the server persists it and re-broadcasts the `spoken_tokens` message on
  change; a missing file is seeded with the built-in "hey buddy" wake family + the
  "beep" end token, then the app owns it — the configured list fully REPLACES the old
  built-in wake word),
  `SPAWNER_HOSTS` (`hosts.json`; the
  app-managed SSH host registry — the app is the source of truth, this file just persists it),
  `SPAWNER_IDENTITIES` (`identities.json`; the app-managed SSH identity registry — names + public
  keys), `SPAWNER_SSH_KEYS` (`ssh_keys`; directory holding each identity's private key, `0600`; the
  private material never leaves the server, the app only sees/copies the public key),
  `SPAWNER_CLAUDE_BIN` (`claude`; the host binary for Claude-backend sessions — the first entry
  in the AI backend registry, see `docs/architecture.md`. Codex's per-target binaries are
  `SPAWNER_SSH_CODEX_BIN` for host/SSH turns and `SPAWNER_SANDBOX_CODEX_BIN` for the sandbox;
  opencode's are `SPAWNER_SSH_OPENCODE_BIN` (host/SSH, default `opencode`) and
  `SPAWNER_SANDBOX_OPENCODE_BIN` (sandbox, default `opencode`). The opencode backend drives local
  Ollama models — its model catalogue is `ollama/*`, resolved via the provider block in the host
  user's `~/.config/opencode/opencode.jsonc`, which must point at the running Ollama server.
  Antigravity's (Google's Gemini-powered `agy` CLI) per-target binaries are `SPAWNER_SSH_AGY_BIN`
  (host/SSH, default `agy`) and `SPAWNER_SANDBOX_AGY_BIN` (sandbox, default `agy`). Antigravity is
  driven non-interactively via `agy --prompt` — it has no machine-readable stream mode, so only the
  final spoken reply is captured (no live tool events or token accounting). agy mints its own
  conversation id; the server discovers it after the first turn and resumes with `--conversation`.
- Background-job notifier: `SPAWNER_EAGER_NOTIFY` (`false`; when true, the idle notifier drives its
  autonomous "your background job finished" turn the moment a detached job is detected **even with
  no device attached**, instead of holding the note until the next attach/dictation — so the agent
  isn't idle for the gap until you revisit the session, and the follow-up is far likelier to land
  inside the token cache window. The spoken reply buffers in the hub's orphan slot for the next
  attach. Default `false` keeps the conservative "never narrate to an empty room" behavior).
- Transport TLS (all optional; empty = plain `ws://`, fine behind Tailscale): `SPAWNER_TLS_CERT`
  and `SPAWNER_TLS_KEY` (PEM cert/key — set **both** to serve `wss://`; one without the other is a
  startup error), `SPAWNER_TLS_CLIENT_CA` (PEM CA bundle — when set, the app must present a client
  certificate signed by one of these CAs **in addition to** the token → mutual TLS; requires the
  cert/key pair).
- CLI STT: `SPAWNER_WHISPER_BIN` (`whisper-cli`), `SPAWNER_WHISPER_MODEL` (path; enables STT),
  `SPAWNER_WHISPER_MODEL_FAST` / `SPAWNER_WHISPER_MODEL_BASE` (per-size model paths for the
  clip-length model picker), `SPAWNER_WHISPER_LANG` (`en`), `SPAWNER_FFMPEG_BIN` (`ffmpeg`).
- Resident-server STT: `SPAWNER_WHISPER_URL` (accurate server), `SPAWNER_WHISPER_FAST_URL` (fast
  draft/detection server), `SPAWNER_WHISPER_MODEL_NAME` (`medium.en`; reported to clients),
  `SPAWNER_WHISPER_FAST_MODEL_NAME` (`base.en`; the fast server's boot model, same lifecycle),
  `SPAWNER_WHISPER_MODELS_DIR` (the host directory of ggml model files the whisper containers
  mount at `/models`; when set, its model names are sent to clients as a settings picker —
  empty = free-text entry only),
  `SPAWNER_WHISPER_FAST_MAX_SEC` (`2.5`; clips shorter than this use the fast server).
- Dedicated wake-word / end-token detector (the LiveKit epic, see `TODO.toml`): `SPAWNER_WAKEWORD_URL`
  (base URL of the resident `spawner-wakeword` sidecar, e.g. `http://localhost:9060` — the Rust
  service wrapping LiveKit's runtime; it slides a 2s window over each clip and returns peak per-model
  scores, `POST /detect`). When set, live hands-free wake ("bump bump") / end ("beep beep") detection
  scores the dedicated model instead of fast-transcribing the clip and string-matching; empty
  disables it and detection falls back to the Whisper string-match. Accurate commit transcription is
  unaffected either way. `SPAWNER_WAKEWORD_THRESHOLD` (`0.5`; the score in `[0,1]` at/above which a
  token counts as detected — the trained models' optimal point is ~`0.04`–`0.07`, so lowering it
  trades a few false positives for near-zero misses). Detector *models* are trained out-of-tree —
  see the training project at `/data/livekit_training` (the app only consumes a finished model).
- Server-side denoise: `SPAWNER_DENOISE_URL` (base URL of the resident DeepFilterNet denoise
  sidecar, e.g. `http://localhost:8573` — the `deepfilternet` service in `/data/speech_services`,
  `POST /denoise`). When set, the server advertises server-side denoising to clients (`hello_ok`
  `denoise`); for a client that opts in (`hello` `denoise`), each clip's PCM is scrubbed of steady
  background noise (wind, road, engine, fan) at the accurate-transcribe seam before Whisper — one
  seam covers push-to-talk, calibration, the hands-free draft, the end-token detector and the commit
  re-transcribe. The client's `denoise_atten_db` caps the attenuation (DeepFilterNet `atten_lim_db`;
  lower is gentler, `<=0`/unset = full). Empty disables it (clips transcribed unfiltered); any
  sidecar failure falls back to the original clip rather than failing the turn.
- Server-side TTS (the Kokoro epic, see `TODO.toml`): `SPAWNER_TTS_URL` (base URL of the resident
  Kokoro-FastAPI server, e.g. `http://localhost:8880` — the `kokoro` compose service; empty
  disables server TTS and clients use on-device speech), `SPAWNER_TTS_VOICE` (`af_heart`; default
  Kokoro voice until a client picks one), `SPAWNER_TTS_FORMAT` (`opus`; synthesis response format:
  mp3 | wav | opus | flac | pcm).
- Sandbox sessions (per-session `target: sandbox` execution): `SPAWNER_SANDBOX_IMAGE` (container
  image; **empty disables** the sandbox target), `SPAWNER_SANDBOX_RUNTIME` (`podman`; the container
  CLI — rootless so no host root), `SPAWNER_SANDBOX_CLAUDE_BIN` (`claude`; the binary inside the
  image), `SPAWNER_SANDBOX_CODEX_BIN` (`codex`; the codex binary inside the image for Codex-backend
  sandbox sessions), `SPAWNER_SANDBOX_MOUNTS` (comma-separated extra `-v` specs, e.g. sharing `$HOME/.claude`),
  `SPAWNER_SANDBOX_RUN_ARGS` (space-separated extra `run` flags, e.g. `--userns=keep-id`).
- SSH-native execution (**unconditional** — every host-target turn, local included, runs over SSH
  with no special-cased localhost fork, and the sandbox's podman + transcript reads run over the
  same pool on the loopback host; the running server never touches its own filesystem for Claude
  state. The direct-fork `HostExecutor` survives only as the hermetic unit-test executor, never in
  production): `SPAWNER_SSH_USER` (login user; empty = current OS user), `SPAWNER_SSH_PORT` (`22`),
  `SPAWNER_SSH_KEY` (private-key path; **empty = the server self-manages its OWN keypair**, minting an
  ed25519 key under the state dir (`<state>/ssh/id_ed25519`) on first boot and writing the public key
  to `<key>.pub` + logging it — install that in the target host's `~/.ssh/authorized_keys` to grant
  access; a set path overrides and is used as-is), `SPAWNER_SSH_KNOWN_HOSTS`
  (`~/.ssh/known_hosts`; host keys are always verified — no insecure mode. The server **owns**
  this file and **auto-seeds** it: the loopback host is trusted on first boot, adding a host in the
  app records its key trust-on-first-use, deleting the host forgets it, and the running pool reloads
  the file so it takes effect without a restart), `SPAWNER_SSH_CLAUDE_BIN`
  (`claude`; the remote claude binary), `SPAWNER_SSH_CODEX_BIN` (`codex`; the remote codex binary for
  Codex-backend SSH sessions — SSH reuses the host target, so this is the host codex binary).
- Claude context trimming: `SPAWNER_CLAUDE_EXTRA_ARGS` (space-separated extra flags appended to
  **every** Claude turn and the `/usage` probe; empty = no-op default, unchanged behavior). This is
  the knob for shrinking the per-turn context Claude Code sends — the fixed overhead is ~20k tokens
  on a bare turn (its system prompt + built-in tool schemas + the injected skills listing), on top
  of any project `CLAUDE.md`/memory. Note the auth constraint: Claude's own `--bare` minimal mode
  refuses OAuth (it accepts only `ANTHROPIC_API_KEY`/apiKeyHelper), so it is **not** usable here —
  these flags are the OAuth-compatible way to trim. Measured savings in an empty dir (single "reply
  Pong" turn, baseline ~20.6k): `--disable-slash-commands` drops the skills listing (~3.5k with
  `--setting-sources`); `--setting-sources project` (or `""`) skips the user/global `settings.json`;
  disallowing built-in tools by name (e.g. `--disallowedTools NotebookEdit WebFetch WebSearch`)
  removes those tool schemas — **`--allowedTools` does NOT shrink context**, it only permission-gates
  while keeping every schema, so use `--disallowedTools` to actually drop them. Each carries a
  capability tradeoff (no skills, no user settings, fewer tools), so it is off by default and opt-in
  per deployment; a capable coding session should trim only tools/skills a voice turn never uses, not
  strip everything. A recommended moderate value: `--disable-slash-commands --setting-sources project`.
- Restart: `SPAWNER_RESTART_CMD` — a shell command fired by the app's restart button; empty disables
  restart. The server runs in a Docker container that builds the Go binary and drives the host over
  SSH (host `claude` turns and the rootless sandbox runtime both execute on the host — no separate
  host broker). The button does a **rebuild+recreate**: it runs `deploy/rebuild-container.sh`
  detached **on the host**, which must run there because a recreate replaces the very container the
  server runs in — an in-container command would be killed mid-recreate, so `setsid` decouples it.
  The server runs the command on the host over its own Go-native SSH connection pool (no openssh
  client — the container needs no `/etc/passwd` entry). The restart message carries a **`mode`**
  (three buttons in the app): the server substitutes the `%REBUILD%` token in the command with the
  mode and passes it to the script — **`build`** rebuilds the image only and leaves the running
  container in place (the live session isn't bounced; the new image is staged for a later restart),
  **`bounce`** recreates the container from the existing image (fast, no code change, no rebuild),
  and **`rebuild`** (the default, and the voice command) does a `--no-cache` recompile then recreate.
  Commands with no `%REBUILD%` token always rebuild. See `deploy/README.md`.

# Android app (Kotlin / Jetpack Compose)

The claude_spawner phone client. Talks the WebSocket protocol in
[`../docs/protocol.md`](../docs/protocol.md): connect + authenticate, push-to-talk voice (captured
as PCM16 / 16 kHz / mono, **encoded to Ogg/Opus on device** before sending), typed utterances, and
it reads the server's `say`/`output` replies aloud (TTS).

## What works today

- **Connect** to the gateway (URL + token) with live status; auto-reconnect.
- **Push-to-talk**: hold the mic button to stream Opus audio → server transcribes (whisper.cpp) →
  reply.
- **Hands-free** (always-listening): a mic `VoiceService` streams VAD-gated speech; wake word and
  end token are detected in the transcript (no on-device Porcupine). Draft shown live. Turn it **on**
  by **holding the mic button and dragging up** — a drag track appears above the button showing how
  far to go, filling toward a 🎧 target; reaching it abandons the in-progress push-to-talk clip and
  flips into hands-free. While hands-free is on the button is a **red headset** (so it's obvious the
  mic is live); **tap it once to turn hands-free back off**. No separate top-bar switch.
- **Type an utterance** to drive the whole spawn/attach/dictate flow without a mic.
- **Chat UI**: per-session logs + server-served history, drawer session list (with ⚙️ busy flags),
  visual directory browser for new sessions with compact host/profile/provider/model dropdowns,
  rename/delete.
- **TTS**: replies read aloud (markdown stripped first), with an **audio-output picker**
  (earpiece/speaker/Bluetooth) and a **brief-reply** toggle; "hey bud stop" barge-in.
- **Turn controls**: abort a running turn (⏹), post-turn diff summary, and a notification when a
  turn finishes while the app is backgrounded.
- **Commands screen**: a Settings page lists every "hey buddy" command (name, description,
  aliases) and lets you add per-command alias fixups. The list is **generated at build time** from
  `../docs/commands.json` (see the `generateCommands` Gradle task below), so the app can never
  drift from the server's command registry. **To surface a new/changed server command in the app:**
  regenerate the JSON on the server side (`go run ./cmd/gencommands` — see the repo `CLAUDE.md`),
  then rebuild the APK. That's it — no Kotlin change; the generated `Commands.kt` (a build artifact
  under `app/build/`, not committed) picks up the new command on the next build.
- **Settings → Server**: URL/token, the server-global Whisper-model picker, and a **Restart Server**
  button (with a confirm dialog). Restart makes the server process exit so its supervisor rebuilds
  and relaunches it on current code — handy after a server change lands — and the app reconnects on
  its own. Enabled only while connected.

## Remaining

Open work (verifying the hands-free voice model on a real device, etc.) is tracked in the repo's
[`../TODO.md`](../TODO.md), the single live task list.

## Layout

One Kotlin Multiplatform module (`app`) with three source sets (there is no `src/main/java`):

```
app/src/commonMain/kotlin/   shared UI + logic: every composable, VoiceController,
                             net/Protocol.kt (wire messages), net/SpawnerClient.kt
                             (Ktor WebSocket transport), Prefs, tts/Markdown.kt
app/src/androidMain/kotlin/  Android-only: MainActivity, Opus/VAD recorders, Android TTS,
                             SettingsStore (SharedPreferences), VoiceService, Notifier
app/src/wasmJsMain/kotlin/   browser-only client — see ../docs/web-client.md
build.gradle.kts             `generateCommands` task: docs/commands.json -> generated Commands.kt
```

## Build

All builds need JDK 17 or newer on `JAVA_HOME` (the Gradle 8.x wrapper handles the rest).

**Web bundle** (`./gradlew :app:wasmJsBrowserDistribution`): needs **no Android SDK** — works on a
fresh clone; the first build downloads Gradle/Node/Binaryen from the network (~6 min). Output lands
in `app/build/dist/wasmJs/productionExecutable`. See [`../docs/web-client.md`](../docs/web-client.md).

**APK** (`./gradlew :app:assembleDebug`): additionally needs an Android SDK (platform 35,
build-tools 35) — without one the build fails with "SDK location not found". Android Studio
provides it, or bootstrap headless: install the [command-line tools](https://developer.android.com/studio#command-line-tools-only),
set `ANDROID_HOME` (or `sdk.dir` in `local.properties`), then

```bash
sdkmanager "platforms;android-35" "build-tools;35.0.0"
sdkmanager --licenses
```

Or the containerized build used in this repo, run from the **repo root** (the whole repo must be
mounted, not just `android/` — the build reads the repo-root `docs/commands.json`). All mounted
host dirs must **pre-exist** — Docker creates missing bind sources root-owned, breaking the
non-root build — and `~/Android/Sdk` must be a provisioned SDK as above. The Gradle cache is a
dedicated dir rather than `~/.gradle`: mounting the host's would leak its `gradle.properties`
(e.g. a host-only `org.gradle.java.home`) into the container and clash with host Gradle daemons
on the cache journal lock:

```bash
mkdir -p ~/.cache/claude_spawner-gradle
docker run --rm --user "$(id -u):$(id -g)" \
  -e HOME=/gradlehome -e GRADLE_USER_HOME=/gradlehome -e ANDROID_SDK_ROOT=/sdk \
  -v "$PWD:/project" -v "$HOME/Android/Sdk:/sdk" \
  -v "$HOME/.cache/claude_spawner-gradle:/gradlehome" \
  -w /project/android gradle:8.10.2-jdk17 gradle :app:assembleDebug --no-daemon
# -> android/app/build/outputs/apk/debug/app-debug.apk
```

Set `SPAWNER_DEBUG_KEYSTORE` to a keystore path to pin the debug signing key (see
`app/build.gradle.kts`); without it each environment mints its own debug key, so an
`adb install -r` over an APK from a different build environment fails with a signature mismatch.

## Run

Any Android device or standard emulator over adb works: `adb install -r app-debug.apk`.

On first run, set **your** server URL + token in Settings → Server — the baked-in defaults are the
maintainer's private dev values, not anything a fresh install can reach.

### Maintainer-specific: the headless emulator

The `android-emulator` container below comes from the maintainer's private `/data/android` setup,
not this repo — use any emulator/device instead:

```bash
docker cp app/build/outputs/apk/debug/app-debug.apk android-emulator:/tmp/
docker exec android-emulator adb install -r /tmp/app-debug.apk
docker exec android-emulator adb shell monkey -p com.bam.spawner -c android.intent.category.LAUNCHER 1
```

## Config / permissions

- `INTERNET` (WebSocket transport); `RECORD_AUDIO` (requested on launch) for the mic;
  `POST_NOTIFICATIONS` for turn notifications; `BLUETOOTH_CONNECT` (requested when you pick
  Bluetooth output); `FOREGROUND_SERVICE` + `FOREGROUND_SERVICE_MICROPHONE` for the hands-free
  background mic service.
- Push-to-talk / hands-free send Ogg/Opus; the server decodes to PCM16LE / 16 kHz / mono.

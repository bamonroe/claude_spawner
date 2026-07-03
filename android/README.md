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
  end token are detected in the transcript (no on-device Porcupine). Draft shown live. Toggle it by
  **holding the mic button and swiping up** — no separate top-bar switch. Swiping up during a
  push-to-talk hold abandons that clip and flips hands-free instead; swiping up while hands-free is
  on turns it back off.
- **Type an utterance** to drive the whole spawn/attach/dictate flow without a mic.
- **Chat UI**: per-session logs + server-served history, drawer session list (with ⚙️ busy flags),
  visual directory browser for new sessions, rename/delete.
- **TTS**: replies read aloud (markdown stripped first), with an **audio-output picker**
  (earpiece/speaker/Bluetooth) and a **brief-reply** toggle; "hey bud stop" barge-in.
- **Turn controls**: abort a running turn (⏹), post-turn diff summary, and a notification when a
  turn finishes while the app is backgrounded.
- **Commands screen**: a Settings page lists every "hey buddy" command (name, description,
  aliases) and lets you add per-command alias fixups. The list is **generated at build time** from
  `../docs/commands.json` (see the `generateCommands` Gradle task below), so the app can never
  drift from the server's command registry.

## Remaining

Open work (verifying the hands-free voice model on a real device, etc.) is tracked in the repo's
[`../TODO.md`](../TODO.md), the single live task list.

## Layout

```
app/src/main/java/com/bam/spawner/
  MainActivity.kt         Compose UI (chat, drawer, settings, browser, commands screen)
  VoiceController.kt      orchestration: WS + audio + TTS -> UI state
  Spawner.kt              process-wide singleton holding the shared VoiceController
  SettingsStore.kt        server URL / token / voice prefs (SharedPreferences)
  Notifier.kt             backgrounded turn-completion notifications
  net/Protocol.kt         ServerMsg parsing + Outbound builders (org.json)
  net/SpawnerClient.kt    OkHttp WebSocket transport (+ keepalive)
  audio/HandsFreeRecorder.kt  always-listening VAD capture -> Opus clips
  audio/OpusRecorder.kt   push-to-talk Opus capture
  audio/OpusOggEncoder.kt PCM16 -> Ogg/Opus encoder (shared by both recorders)
  audio/Endpointer.kt     RMS VAD (onset/silence)
  audio/AudioRouter.kt    earpiece/speaker/Bluetooth output routing
  audio/LevelMeter.kt     mic level meter for the audio settings page
  tts/Speaker.kt          TextToSpeech wrapper (VOICE_COMMUNICATION)
  tts/Markdown.kt         strip markdown -> plain text before speaking
  ui/MarkdownText.kt      Compose renderer for markdown in the chat log
  ui/Theme.kt             Material3 theme (system/light/dark)
  service/VoiceService.kt  foreground mic service for background listening
build.gradle.kts          `generateCommands` task: docs/commands.json -> generated Commands.kt (COMMANDS)
```

## Build

Needs Android SDK (platform 35, build-tools 35) and JDK 17. Easiest is Android Studio, or the
containerized build used in this repo (matches the host's SDK, avoids a JDK-version mismatch):

```bash
docker run --rm --user "$(id -u):$(id -g)" \
  -e HOME=/gradlehome -e GRADLE_USER_HOME=/gradlehome -e ANDROID_SDK_ROOT=/sdk \
  -v "$PWD:/project" -v "$HOME/Android/Sdk:/sdk" -v "$HOME/.gradle:/gradlehome" \
  -w /project gradle:8.10.2-jdk17 gradle :app:assembleDebug --no-daemon
# -> app/build/outputs/apk/debug/app-debug.apk
```

## Run on the headless emulator (this repo's `/data/android`)

```bash
docker cp app/build/outputs/apk/debug/app-debug.apk android-emulator:/tmp/
docker exec android-emulator adb install -r /tmp/app-debug.apk
docker exec android-emulator adb shell monkey -p com.bam.spawner -c android.intent.category.LAUNCHER 1
```

The default server URL is `ws://100.64.0.2:8555/ws` (the host's tailnet IP), token `devsecret` —
change it in Settings → Server. NOTE: from the Dockerized emulator the host is reachable at that
tailnet IP, not `10.0.2.2` (which is the emulator's container).

## Config / permissions

- `INTERNET` (WebSocket transport); `RECORD_AUDIO` (requested on launch) for the mic;
  `POST_NOTIFICATIONS` for turn notifications; `BLUETOOTH_CONNECT` (requested when you pick
  Bluetooth output); `FOREGROUND_SERVICE` + `FOREGROUND_SERVICE_MICROPHONE` for the hands-free
  background mic service.
- Push-to-talk / hands-free send Ogg/Opus; the server decodes to PCM16LE / 16 kHz / mono.

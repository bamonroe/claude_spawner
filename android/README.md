# Android app (Kotlin / Jetpack Compose)

The claude_spawner phone client. Talks the WebSocket protocol in
[`../docs/protocol.md`](../docs/protocol.md): connect + authenticate, push-to-talk audio (PCM16 /
16 kHz / mono), typed utterances, and it reads the server's `say`/`output` replies aloud (TTS).

## What works today

- **Connect** to the gateway (URL + token) with live status; auto-reconnect.
- **Push-to-talk**: hold to stream Opus audio → server transcribes (whisper.cpp) → reply.
- **Hands-free** (always-listening): a mic `VoiceService` streams VAD-gated speech; wake word and
  end token are detected in the transcript (no on-device Porcupine). Draft shown live.
- **Type an utterance** to drive the whole spawn/attach/dictate flow without a mic.
- **Chat UI**: per-session logs + server-served history, drawer session list (with ⚙️ busy flags),
  visual directory browser for new sessions, rename/delete.
- **TTS**: replies read aloud, with an **audio-output picker** (earpiece/speaker/Bluetooth) and a
  **brief-reply** toggle; "hey bud stop" barge-in.
- **Turn controls**: abort a running turn (⏹), post-turn diff summary, and a notification when a
  turn finishes while the app is backgrounded.

## Remaining

- Verify the hands-free voice model on a real device (built, not yet voice-tested end-to-end).

## Layout

```
app/src/main/java/com/bam/spawner/
  MainActivity.kt         Compose UI (chat, drawer, settings, browser)
  VoiceController.kt      orchestration: WS + audio + TTS -> UI state
  SettingsStore.kt        server URL / token / voice prefs (SharedPreferences)
  Notifier.kt             backgrounded turn-completion notifications
  net/Protocol.kt         ServerMsg parsing + Outbound builders (org.json)
  net/SpawnerClient.kt    OkHttp WebSocket transport (+ keepalive)
  audio/HandsFreeRecorder.kt  always-listening VAD capture -> Opus clips
  audio/OpusRecorder.kt   push-to-talk Opus capture
  audio/Endpointer.kt     RMS VAD (onset/silence)
  audio/AudioRouter.kt    earpiece/speaker/Bluetooth output routing
  tts/Speaker.kt          TextToSpeech wrapper (VOICE_COMMUNICATION)
  service/VoiceService.kt  foreground mic service for background listening
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

- `RECORD_AUDIO` (requested on launch) for the mic; `POST_NOTIFICATIONS` for turn notifications;
  `BLUETOOTH_CONNECT` (requested when you pick Bluetooth output).
- Push-to-talk / hands-free send Ogg/Opus; the server decodes to PCM16LE / 16 kHz / mono.

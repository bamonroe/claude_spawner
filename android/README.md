# Android app (Kotlin / Jetpack Compose)

The claude_spawner phone client. Talks the WebSocket protocol in
[`../docs/protocol.md`](../docs/protocol.md): connect + authenticate, push-to-talk audio (PCM16 /
16 kHz / mono), typed utterances, and it reads the server's `say`/`output` replies aloud (TTS).

## What works today

- **Connect** to the gateway (URL + token), with live status.
- **Push-to-talk**: hold the button to stream mic audio → server transcribes (whisper.cpp) →
  transcript + dialog come back.
- **Type an utterance**: text box mirrors the `wsclient` tool — drive the whole spawn/attach/
  dictate flow without a mic.
- **Text-to-speech**: `say` prompts and Claude `output` are spoken.
- Transcript/response **log** view.

## Not wired yet (stubs in place)

- **Wake word** "hey buddy" (`wake/WakeWordController.kt`) — push-to-talk is the current trigger.
  Porcupine dependency is declared; needs a Picovoice access key + a custom `hey_buddy.ppn`.
- **Background always-listening** (`service/VoiceService.kt`) — foreground-service plumbing is
  there; the wake listener will move into it so it works with the screen off.
- Automatic endpointing (`audio/Endpointer.kt`) exists for the wake path but PTT uses release.

## Layout

```
app/src/main/java/com/bam/spawner/
  MainActivity.kt        Compose UI (connect, PTT, text input, log)
  VoiceController.kt      orchestration: WS + audio + TTS -> UI state
  SettingsStore.kt        server URL / token / picovoice key (SharedPreferences)
  net/Protocol.kt         ServerMsg parsing + Outbound builders (org.json)
  net/SpawnerClient.kt    OkHttp WebSocket transport
  audio/AudioCapture.kt   AudioRecord -> PCM16 frames
  audio/Endpointer.kt     RMS silence detection (wake path)
  tts/Speaker.kt          TextToSpeech wrapper
  wake/WakeWordController  Porcupine stub (TODO)
  service/VoiceService.kt  foreground-service stub (TODO)
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

The default server URL is `ws://10.0.2.2:8080/ws` (`10.0.2.2` = host loopback from the emulator),
token `devsecret`. Point it at the running server container's published port.

## Config / permissions

- Requires `RECORD_AUDIO` (requested on launch) for push-to-talk.
- Audio format is fixed to PCM16LE / 16 kHz / mono to match the server.

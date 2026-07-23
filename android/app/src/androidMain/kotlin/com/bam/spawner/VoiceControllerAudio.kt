package com.bam.spawner

import android.content.Context
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageReport
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.audio.AudioRouter
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.audio.HandsFreeRecorder
import com.bam.spawner.audio.LevelMeter
import com.bam.spawner.audio.OpusRecorder
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ProfileInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SpawnerClient
import com.bam.spawner.tts.Markdown
import com.bam.spawner.tts.Speaker
import java.io.File
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// --- Audio input / output routing, hands-free VAD, level meter, silence-commit,
//     end-token calibration, and push-to-talk ---
// Extension functions on VoiceController (identical to member functions; split out
// only to shrink the class file). Any state they touch is `internal` on VoiceController.

// How to capture + play during hands-free, resolved from the current output route.
internal data class MicProfile(val commMode: Boolean, val source: Int, val aec: Boolean, val ns: Boolean)

// Apply an output: MUTE suppresses TTS (no device routing); anything else
// unmutes and routes the device. Returns whether it took effect.
internal fun VoiceController.applyAudioOutput(out: AudioOutput): Boolean =
    if (out == AudioOutput.MUTE) { cancelServerSpeech(); speaker.setMuted(true); true }
    else { speaker.setMuted(false); audioRouter.setOutput(out) }

internal suspend fun VoiceController.applyAudioOutputVerified(out: AudioOutput): Boolean {
    if (out == AudioOutput.MUTE) return applyAudioOutput(out)
    repeat(3) {
        if (applyAudioOutput(out)) {
            repeat(8) {
                delay(250)
                if (audioRouter.outputActive(out)) return true
            }
        } else {
            delay(250)
        }
    }
    return false
}

/** Re-scan available outputs (call when opening the picker to catch a
 *  just-connected/removed Bluetooth headset). */
internal fun VoiceController.refreshAudioOutputs() {
    val avail = audioRouter.available()
    _audioOutputs.value = avail
    _audioInputs.value = audioRouter.availableInputs()
    // If the selected device vanished (e.g. the Bluetooth headset disconnected),
    // fall back to earpiece. MUTE is always available.
    val cur = _audioOutput.value
    if (cur != AudioOutput.MUTE && cur !in avail) setAudioOutput(AudioOutput.EARPIECE)
    // Likewise, if the headset mic went away, fall back to the device mic.
    if (_audioInput.value == AudioInput.HEADSET && AudioInput.HEADSET !in _audioInputs.value) {
        setAudioInput(AudioInput.DEVICE)
    }
}

/** Choose the capture (mic) source and remember it. Capture is route-dependent,
 *  so re-resolve the mic profile live while listening. */
internal fun VoiceController.setAudioInput(inp: AudioInput) {
    _audioInput.value = inp
    settings.micSource = inp.pref
    // An explicit pick is a deliberate (re)try, so clear any prior SCO-failure
    // latch: re-selecting Headset must re-attempt the Bluetooth link rather than
    // stay silently on the built-in mic (the latch otherwise only clears when the
    // input *value* changes, forcing a Device→Headset round-trip).
    headsetMicFailed = false
    if (hfOn) restartHandsFree()
    _audioInputs.value = audioRouter.availableInputs()
}

/** Route the spoken audio to [out] (or mute) and remember the choice. */
internal fun VoiceController.setAudioOutput(out: AudioOutput) {
    val request = audioOutputRequest.incrementAndGet()
    scope.launch {
        if (!applyAudioOutputVerified(out)) {
            if (request == audioOutputRequest.get()) {
                applyAudioOutput(_audioOutput.value)
                _audioOutputs.value = audioRouter.available()
                _mic.value = "⚠️ audio route unavailable"
            }
            return@launch
        }
        if (request != audioOutputRequest.get()) return@launch
        _audioOutput.value = out
        settings.audioOutput = out.name.lowercase()
        // Capture is route-dependent (comm-audio vs media, headset vs built-in mic),
        // so re-resolve the mic profile against the new output while listening.
        if (hfOn) restartHandsFree()
        _audioOutputs.value = audioRouter.available()
    }
}

internal fun VoiceController.vadConfig() = com.bam.spawner.audio.VadConfig(
    rmsThreshold = settings.vadThreshold.toDouble(),
    onsetMs = settings.vadOnsetMs,
    silenceMs = settings.vadSilenceMs,
    adaptive = settings.vadAdaptive,
)

/** Resolve how to capture + play for hands-free straight from the two explicit
 *  picks — the [AudioInput] mic source and the [AudioOutput] route — with no
 *  inference:
 *  - Headset input + a Bluetooth headset present → grab its hands-free (SCO)
 *    profile: comm audio + AEC, headset mic, from across the room (call quality).
 *    The SCO link carries playback too, so it overrides the output route.
 *  - Device input + headset output → plain media capture, no AEC: our TTS is in
 *    the user's ears (nothing to echo-cancel) and staying out of call mode stops
 *    Android from ducking other apps' audio (e.g. a movie) to a whisper.
 *  - Device input + earpiece/speaker/mute → comm audio + echo canceller so voice
 *    barge-in works over the speaker. */
internal fun VoiceController.resolveMicProfile(): MicProfile {
    // Any change to the input pick clears a prior SCO-failure latch, so explicitly
    // re-selecting the headset retries it; an unchanged value (e.g. a route-change
    // restart) keeps the latch so we don't loop on a dead link.
    val inputPref = _audioInput.value.pref
    if (inputPref != lastMicSource) { headsetMicFailed = false; lastMicSource = inputPref }
    val useHeadset =
        _audioInput.value == AudioInput.HEADSET && !headsetMicFailed && audioRouter.bluetoothMicAvailable()
    if (useHeadset && !headsetMicOn) { headsetMicOn = audioRouter.enableHeadsetMic() }
    else if (!useHeadset && headsetMicOn) { audioRouter.disableHeadsetMic(); headsetMicOn = false }
    headphonesRoute = audioRouter.headphonesConnected()
    // Headset mic (SCO) → call-mode capture. Otherwise the device mic, whose
    // profile follows the output route.
    return when {
        headsetMicOn -> MicProfile(true, android.media.MediaRecorder.AudioSource.VOICE_COMMUNICATION, true, true)
        // Headset/media path: AEC stays off (TTS is in the user's ears), but the
        // noise suppressor is an independent opt-in for filtering ambient noise.
        _audioOutput.value == AudioOutput.HEADSET ->
            MicProfile(false, android.media.MediaRecorder.AudioSource.VOICE_RECOGNITION, false, settings.headsetNoiseSuppression)
        else -> MicProfile(true, android.media.MediaRecorder.AudioSource.VOICE_COMMUNICATION, true, true)
    }
}

internal fun VoiceController.newHandsFree(profile: MicProfile) = HandsFreeRecorder(
    app, vadConfig(), this::onHandsFreeSpeechStart, this::onHandsFreeUtterance,
    { _micLevel.value = it }, profile.source, profile.aec, profile.ns,
)

/** Starts the always-listening pipeline. Returns false if the mic is unavailable. */
internal fun VoiceController.startHandsFree(): Boolean {
    if (hfOn) return true
    if (recording) return false // don't fight push-to-talk for the mic
    stopMeter() // free the mic if the level meter was running
    headsetMicFailed = false // a fresh enable gets one clean SCO attempt
    val profile = resolveMicProfile()
    val hf = newHandsFree(profile)
    if (!hf.start()) {
        _mic.value = "⚠️ mic unavailable"
        return false
    }
    handsFree = hf
    hfOn = true
    speaker.setCommMode(profile.commMode)
    _voiceState.value = VoiceState.LISTENING
    _mic.value = "🟢 listening for \"hey buddy\"…"
    if (headsetMicOn) scope.launch { verifyHeadsetMic() }
    return true
}

/** Re-apply VAD settings / audio route live (restart the recorder) if hands-free
 *  is running. */
internal fun VoiceController.restartHandsFree() {
    if (!hfOn) return
    handsFree?.stop()
    val profile = resolveMicProfile()
    val hf = newHandsFree(profile)
    if (hf.start()) {
        handsFree = hf
        speaker.setCommMode(profile.commMode)
        _voiceState.value = VoiceState.LISTENING
        if (headsetMicOn) scope.launch { verifyHeadsetMic() }
    } else {
        handsFree = null; hfOn = false; _voiceState.value = VoiceState.OFF
    }
}

/** After grabbing a Bluetooth headset's hands-free profile, give the SCO link a
 *  moment to actually come up. If it didn't (some earbuds refuse it on demand and
 *  the platform silently reverts to the mic-less A2DP link), latch the failure and
 *  restart on the built-in mic so the user is never left unheard. */
internal suspend fun VoiceController.verifyHeadsetMic() {
    // Poll the SCO link rather than judging it once: car kits and some earbuds
    // take several seconds to bring the hands-free profile up, and a single early
    // check wrongly latched failure and dropped to the built-in mic. Give it up to
    // ~6 s, succeeding the moment the link is live.
    repeat(12) {
        delay(500)
        if (!hfOn || !headsetMicOn) return
        if (audioRouter.headsetMicActive()) return // link came up — keep the headset mic
    }
    headsetMicFailed = true // gave it a fair window; fall back so the user is heard
    _mic.value = "🟢 listening (headset mic unavailable — using built-in)…"
    restartHandsFree()
}

/** Headphones plugged/unplugged (or Bluetooth connected/dropped): if the route
 *  actually flipped speaker↔headphones while listening, restart capture so the
 *  comm-mode/echo-canceller choice follows it. Runs off the main thread because
 *  restarting joins the capture worker. */
internal fun VoiceController.onAudioRouteChanged() {
    val nowHeadphones = audioRouter.headphonesConnected()
    _audioOutputs.value = audioRouter.available()
    _audioInputs.value = audioRouter.availableInputs()
    // If the headset mic was selected but its headset is gone, fall back to the
    // device mic (setAudioInput restarts capture).
    if (_audioInput.value == AudioInput.HEADSET && AudioInput.HEADSET !in _audioInputs.value) {
        setAudioInput(AudioInput.DEVICE); return
    }
    // Auto-prefer the headset-media output when a headset appears — but only if the
    // user is still on the default earpiece, so an explicit pick is left untouched;
    // fall back off it when the headset goes away. setAudioOutput restarts capture.
    if (nowHeadphones && _audioOutput.value == AudioOutput.EARPIECE) {
        setAudioOutput(AudioOutput.HEADSET); return
    }
    if (!nowHeadphones && _audioOutput.value == AudioOutput.HEADSET) {
        setAudioOutput(AudioOutput.EARPIECE); return
    }
    if (!hfOn) { headphonesRoute = nowHeadphones; return }
    if (nowHeadphones == headphonesRoute) return
    restartHandsFree()
}

internal fun VoiceController.stopHandsFree() {
    hfOn = false
    speaker.setCommMode(false) // back to the regular media speaker
    handsFree?.stop()
    handsFree = null
    if (headsetMicOn) { // release the Bluetooth hands-free profile we grabbed
        audioRouter.disableHeadsetMic(); headsetMicOn = false
        applyAudioOutput(_audioOutput.value) // restore the user's chosen TTS output
    }
    cancelSilenceCommit()
    _micLevel.value = 0.0
    _voiceState.value = VoiceState.OFF
    _mic.value = ""
    // Drop any uncommitted draft: clear it on-screen now, and tell the server
    // to discard its buffered audio so it can't bleed into the next capture.
    if (_pending.value.isNotEmpty()) {
        _pending.value = ""
        if (_connected.value) client?.send(Outbound.discardDraft())
    }
}

// --- Live level meter (Audio settings page) ---
/** Start a standalone meter unless hands-free is already feeding the level. */
internal fun VoiceController.startMeter() {
    if (hfOn || meter != null) return
    val m = LevelMeter(app) { _micLevel.value = it }
    if (m.start()) meter = m
}

internal fun VoiceController.stopMeter() {
    meter?.stop(); meter = null
    if (!hfOn) _micLevel.value = 0.0
}

// --- Silence-commit timeout (client-driven): commit the buffer after N s quiet ---
internal fun VoiceController.scheduleSilenceCommit() {
    cancelSilenceCommit()
    val secs = settings.silenceCommitSeconds
    if (secs <= 0f) return
    commitTimer = scope.launch {
        delay((secs * 1000).toLong())
        client?.send(Outbound.commit())
    }
}

internal fun VoiceController.cancelSilenceCommit() {
    commitTimer?.cancel(); commitTimer = null
}

// --- End-token calibration: say the token N times, measure recognition ---
internal fun VoiceController.startCalibration(rounds: Int = 10) {
    stopCalibration()
    if (hfOn) stopHandsFree() // free the mic
    val token = settings.endToken
    val rec = HandsFreeRecorder(app, vadConfig(), onSpeechStart = {}, onUtterance = { clip ->
        client?.let { c ->
            c.send(Outbound.wake(HandsFreeRecorder.CODEC, calibrate = true))
            c.sendAudio(clip)
            c.send(Outbound.audioEnd())
        }
    })
    if (rec.start()) {
        calibRecorder = rec
        _calibration.value = CalibrationState(active = true, token = token, rounds = rounds)
    } else {
        _mic.value = "⚠️ mic unavailable"
    }
}

internal fun VoiceController.stopCalibration() {
    calibRecorder?.stop(); calibRecorder = null
    val st = _calibration.value
    if (st.active) _calibration.value = st.copy(active = false, done = st.samples.isNotEmpty())
}

internal fun VoiceController.onCalibrationSample(text: String) {
    val st = _calibration.value
    if (!st.active) return
    val samples = st.samples + text
    val hits = samples.count { endTokenHit(it, st.token) }
    if (samples.size >= st.rounds) {
        calibRecorder?.stop(); calibRecorder = null
        _calibration.value = st.copy(active = false, done = true, samples = samples, hits = hits)
    } else {
        _calibration.value = st.copy(samples = samples, hits = hits)
    }
}

/** Whole-word match mirroring the server's splitEndToken. */
internal fun VoiceController.endTokenHit(transcript: String, token: String): Boolean {
    val tok = words(token)
    if (tok.isEmpty()) return false
    val ws = words(transcript)
    var i = 0
    while (i + tok.size <= ws.size) {
        if (ws.subList(i, i + tok.size) == tok) return true
        i++
    }
    return false
}

internal fun VoiceController.words(s: String): List<String> =
    s.lowercase().replace(Regex("[,.!?;:\"]"), " ").split(Regex("\\s+")).filter { it.isNotBlank() }

// Called on the capture thread when the user starts speaking.
internal fun VoiceController.onHandsFreeSpeechStart() {
    cancelSilenceCommit() // still talking — don't silence-commit
    // No auto barge-in: speaking does NOT cut off Claude's reply. Only the
    // explicit "hey buddy stop" command halts speech (see ServerMsg.StopSpeaking).
    _voiceState.value = VoiceState.CAPTURING
}

// Called on the capture thread with a finished Opus clip; send it like PTT.
internal fun VoiceController.onHandsFreeUtterance(clip: ByteArray) {
    val c = client ?: return
    c.send(Outbound.wake(HandsFreeRecorder.CODEC, handsFree = true, sessionId = _attachedId.value))
    c.sendAudio(clip)
    c.send(Outbound.audioEnd())
    scheduleSilenceCommit() // start the quiet-timeout after this utterance
    // Back to listening; a server Transcript will bump us to THINKING if acted on.
    if (hfOn) _voiceState.value = VoiceState.LISTENING
}

// --- Push-to-talk (records Opus locally, sends the compressed clip on release) ---

internal fun VoiceController.startTalking() {
    val c = client
    if (c == null || !_connected.value) {
        _mic.value = "⚠️ connect first"
        return
    }
    if (hfOn) return // hands-free owns the mic
    cancelServerSpeech()
    speaker.stop() // barge-in
    if (!recorder.start()) {
        _mic.value = "⚠️ mic unavailable"
        return
    }
    recording = true
    c.send(Outbound.wake(OpusRecorder.CODEC, sessionId = _attachedId.value))
    _mic.value = "🎙️ recording…"
}

internal fun VoiceController.stopTalking() {
    if (!recording) return
    recording = false
    val clip = recorder.stopAndRead()
    if (clip != null && clip.isNotEmpty()) {
        client?.sendAudio(clip)
        _mic.value = "sent ${clip.size / 1024} KB — transcribing…"
    } else {
        _mic.value = "⚠️ no audio captured"
    }
    client?.send(Outbound.audioEnd())
}

/** Abort an in-progress push-to-talk without sending the clip — used when a
 *  swipe-up on the mic button reinterprets the hold as a hands-free toggle.
 *  We don't send `audio_end`; the server's collecting flag self-heals on the
 *  next `wake`, and skipping it avoids a spurious "didn't hear anything." */
internal fun VoiceController.cancelTalking() {
    if (!recording) return
    recording = false
    recorder.stopAndRead() // discard the captured audio
    _mic.value = ""
}

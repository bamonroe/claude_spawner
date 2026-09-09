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
import com.bam.spawner.tts.TtsCatalogue
import com.bam.spawner.tts.TtsEngine
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

// --- Speech synthesis: engine routing, playback, and the on-device fallback ---
// Extension functions on VoiceController (identical to member functions; split out
// only to shrink the class file). Any state they touch is `internal` on VoiceController.

/** Stop the TTS readout (the on-screen tap-to-stop). */
internal fun VoiceController.stopSpeaking() {
    cancelStreamingSpeech()
    speaker.stop()
}

/**
 * Speak [text] (already markdown-stripped) through the engine chosen in
 * Settings › Audio ([Prefs.ttsEngine]).
 *
 * Two of the four engines — server Kokoro and the two local sherpa-onnx models —
 * produce a PCM *stream*, and both feed the same [Speaker] stream path; only the
 * producer differs. The third, `android.speech.tts`, is also the universal
 * fallback: any engine that can't speak right now (server offline or refusing, a
 * local model not installed or failed to load) degrades to it rather than going
 * silent, which is the property that makes switching engines safe to experiment
 * with.
 */
internal fun VoiceController.speakText(text: String) = speakText(text, "")

/**
 * [voice] is engine-relative and optional: the Kokoro voice name for the server
 * and for local Kokoro, ignored by Piper (whose model *is* the voice) and by the
 * Android engine. Blank means "whatever the settings say" — the voice picker
 * passes an explicit one to preview it before it's saved.
 */
internal fun VoiceController.speakText(text: String, voice: String) {
    if (text.isBlank() || speaker.isMuted()) return
    when (settings.ttsEngine) {
        TtsEngine.SERVER -> {
            if (_serverTtsAvailable.value && _connected.value) {
                speakViaServer(text, voice.ifBlank { settings.ttsVoice })
            } else {
                speaker.speak(text)
            }
        }
        TtsEngine.KOKORO, TtsEngine.PIPER -> {
            if (!speakViaLocal(text, voice)) speaker.speak(text)
        }
        else -> speaker.speak(text)
    }
}

/** The server path: it streams PCM back bracketed by speak_audio/speak_end; an
 *  error-bearing speak_end falls back to the on-device voice. */
private fun VoiceController.speakViaServer(text: String, voice: String) {
    val id = synchronized(speakLock) {
        val id = "s${++speakSeq}"
        speakTexts[id] = text
        // Runaway guard; the server refuses past 32 queued anyway.
        while (speakTexts.size > 64) speakTexts.remove(speakTexts.keys.first())
        id
    }
    client?.send(Outbound.speak(id, text, voice = voice, format = "pcm"))
}

/**
 * The on-device path. Returns false when this device can't synthesize right now
 * (no model installed for the chosen engine, or it failed to load), so the
 * caller can fall back to the Android engine.
 *
 * Loading is idempotent and cheap to re-request — [LocalTts.load] no-ops when the
 * same directory is already loaded — so the model stays in sync with the settings
 * without anything having to observe them.
 */
private fun VoiceController.speakViaLocal(text: String, voice: String): Boolean {
    val engine = settings.ttsEngine
    val modelId = when (engine) {
        TtsEngine.KOKORO -> TtsCatalogue.KOKORO_MODEL.id
        else -> settings.piperModel
    }
    val dir = ttsModels.dirFor(modelId) ?: return false
    localTts.load(engine, dir)
    // Kokoro picks a voice by speaker id; an unknown name falls back to the first.
    val sid = if (engine == TtsEngine.KOKORO) {
        TtsCatalogue.KOKORO_VOICES
            .indexOf(voice.ifBlank { settings.kokoroVoice })
            .coerceAtLeast(0)
    } else 0
    // The server may still be mid-utterance from a previous engine choice.
    cancelServerSpeech()
    // The model may still be loading, so "can't speak" surfaces asynchronously —
    // when it does, the Android engine picks the utterance up.
    localTts.speak(text, sid) { speaker.speak(text) }
    return true
}

/** speak_audio: the next binary frames are this utterance's PCM. Anything we
 *  didn't ask for (or a codec we can't stream) is dropped and falls back on
 *  its speak_end. */
internal fun VoiceController.onSpeakAudio(msg: ServerMsg.SpeakAudio) = synchronized(speakLock) {
    speakStreamId = msg.id
    speakStreamLive = msg.codec == "pcm" && speakTexts.containsKey(msg.id)
    if (speakStreamLive) speaker.streamBegin()
}

/** A server→client binary frame — always speak audio (the only binary the
 *  server sends; ordered on the same socket as its speak_audio header). */
internal fun VoiceController.onSpeakFrame(data: ByteArray) {
    val live = synchronized(speakLock) { speakStreamLive }
    if (live) speaker.streamWrite(data)
}

internal fun VoiceController.onSpeakEnd(msg: ServerMsg.SpeakEnd) {
    val wasLive: Boolean
    val text: String?
    synchronized(speakLock) {
        wasLive = speakStreamLive && speakStreamId == msg.id
        if (speakStreamId == msg.id) {
            speakStreamId = null
            speakStreamLive = false
        }
        text = speakTexts.remove(msg.id)
    }
    if (wasLive) speaker.streamEnd()
    // Refused (tts disabled / queue full / synthesis failed) → on-device voice.
    // A stream that died part-way (wasLive) already spoke partially; don't
    // replay the whole utterance on top of it.
    if (msg.error.isNotEmpty() && text != null && !wasLive) speaker.speak(text)
}

/** Voice-picker preview handled by VoiceController.previewTtsVoice (an override). */

/**
 * Silence every streaming engine at once (barge-in, mute, disconnect, detach).
 *
 * There is one cancel for all of them on purpose: callers want "stop talking
 * now" and must not have to know which engine happens to be speaking — that
 * knowledge is exactly what would rot when an engine is added.
 */
internal fun VoiceController.cancelStreamingSpeech() {
    cancelServerSpeech()
    localTts.stop()
}

/** Forget all in-flight server speaks and silence their playback. Frames still
 *  arriving for a cancelled utterance are dropped until its speak_end passes;
 *  speak_stop tells the server to drop its queue and abort the in-flight
 *  synthesis too (moot when disconnected — the outbox just drops it). */
internal fun VoiceController.cancelServerSpeech() {
    val hadInFlight = synchronized(speakLock) {
        val had = speakTexts.isNotEmpty() || speakStreamLive
        speakTexts.clear()
        speakStreamLive = false
        had
    }
    if (hadInFlight) client?.send(Outbound.speakStop())
    speaker.streamStop()
}

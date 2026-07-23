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

// --- Server-TTS (Kokoro) playback + on-device fallback ---
// Extension functions on VoiceController (identical to member functions; split out
// only to shrink the class file). Any state they touch is `internal` on VoiceController.

/** Stop the TTS readout (the on-screen tap-to-stop). */
internal fun VoiceController.stopSpeaking() {
    cancelServerSpeech()
    speaker.stop()
}

/** Speak [text] (already markdown-stripped): with the server's Kokoro voice
 *  when the toggle is on and the server offers TTS, else on-device. The
 *  server streams PCM back bracketed by speak_audio/speak_end; an
 *  error-bearing speak_end falls back to the on-device voice. */
internal fun VoiceController.speakText(text: String) = speakText(text, settings.ttsVoice)

internal fun VoiceController.speakText(text: String, voice: String) {
    if (text.isBlank() || speaker.isMuted()) return
    if (settings.serverTts && _serverTtsAvailable.value && _connected.value) {
        val id = synchronized(speakLock) {
            val id = "s${++speakSeq}"
            speakTexts[id] = text
            // Runaway guard; the server refuses past 32 queued anyway.
            while (speakTexts.size > 64) speakTexts.remove(speakTexts.keys.first())
            id
        }
        client?.send(Outbound.speak(id, text, voice = voice, format = "pcm"))
    } else {
        speaker.speak(text)
    }
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

/** Forget all in-flight server speaks and silence their playback (barge-in,
 *  mute, disconnect). Frames still arriving for a cancelled utterance are
 *  dropped until its speak_end passes; speak_stop tells the server to drop
 *  its queue and abort the in-flight synthesis too (moot when disconnected —
 *  the outbox just drops it). */
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

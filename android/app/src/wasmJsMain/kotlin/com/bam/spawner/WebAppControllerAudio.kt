@file:OptIn(ExperimentalEncodingApi::class)

package com.bam.spawner

import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.Codecs
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.tts.Markdown
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlin.io.encoding.Base64
import kotlin.io.encoding.ExperimentalEncodingApi
import kotlin.js.JsAny
import kotlin.js.JsString

// Split out of WebAppController.kt to keep the mic/speech-playback (push-to-talk,
// hands-free VAD loop, server/browser TTS) helpers in one focused file. Pure
// relocation — see WebAppController.kt for the class fields these extensions
// read/mutate.

/** Switch the browser voice on (Speaker) or off (Mute); Mute also halts any current utterance. */
fun WebAppController.setAudioOutput(out: AudioOutput) {
    val o = if (out == AudioOutput.MUTE) AudioOutput.MUTE else AudioOutput.SPEAKER
    _audioOutput.value = o
    prefs.audioOutput = o.name.lowercase()
    if (o == AudioOutput.MUTE) {
        cancelServerSpeech()
        cancelSpeech()
        _speaking.value = false
    }
}

/** Mic button pressed: barge-in over any speech, then start capturing. */
fun WebAppController.startTalking() {
    if (capturing) return
    cancelServerSpeech(); cancelSpeech(); _speaking.value = false // barge-in
    capturing = true
    _micText.value = "listening…"
    startMic().then<JsAny?> { res: JsString ->
        val s = res.toString()
        if (s.startsWith("err:") && capturing) {
            capturing = false; _micText.value = ""
            addChat(Role.SYSTEM, "⚠️ mic unavailable (${s.removePrefix("err:")})")
        }
        null
    }
}

/** Mic button released: stop, and if we captured anything, ship the clip. */
fun WebAppController.stopTalking() {
    if (!capturing) return
    capturing = false
    _micText.value = ""
    val b64 = stopMic().toString()
    if (b64.isEmpty()) return
    val pcm = Base64.decode(b64)
    client?.send(Outbound.wake(Codecs.PCM16, sessionId = _attachedId.value))
    client?.sendAudio(pcm)
    client?.send(Outbound.audioEnd())
}

/** Swipe-cancel: drop the capture without sending. */
fun WebAppController.cancelTalking() {
    if (!capturing) return
    capturing = false
    _micText.value = ""
    cancelMic()
}

/** Stop-speaking button / "stop" barge-in: halt TTS now. */
fun WebAppController.stopSpeaking() { cancelServerSpeech(); cancelSpeech(); _speaking.value = false }

/** Toggle always-listening on: open the mic under the shared VAD dials, then loop. */
fun WebAppController.startHandsFree() {
    if (handsFreeJob != null) return
    startHandsFreeMic(prefs.vadThreshold, prefs.vadOnsetMs, prefs.vadSilenceMs, HANDS_FREE_MAX_MS)
        .then<JsAny?> { res: JsString ->
            val s = res.toString()
            if (s.startsWith("err:")) {
                _voiceState.value = VoiceState.OFF
                addChat(Role.SYSTEM, "⚠️ mic unavailable (${s.removePrefix("err:")})")
            } else {
                _voiceState.value = VoiceState.LISTENING
                handsFreeJob = scope.launch {
                    while (true) {
                        // Reflect what the mic is doing; SPEAKING (our own TTS) wins so the
                        // pill doesn't flicker to CAPTURING on echo the VAD didn't fully reject.
                        _voiceState.value = when {
                            speechActive() || serverSpeechActive() -> VoiceState.SPEAKING
                            handsFreeCapturing() -> VoiceState.CAPTURING
                            else -> VoiceState.LISTENING
                        }
                        val clip = pollHandsFreeClip().toString()
                        if (clip.isNotEmpty()) {
                            val pcm = Base64.decode(clip)
                            client?.send(Outbound.wake(Codecs.PCM16, handsFree = true, sessionId = _attachedId.value))
                            client?.sendAudio(pcm)
                            client?.send(Outbound.audioEnd())
                        }
                        delay(120)
                    }
                }
            }
            null
        }
}

/** Toggle always-listening off: stop the loop and tear the mic down. */
fun WebAppController.stopHandsFree() {
    handsFreeJob?.cancel(); handsFreeJob = null
    stopHandsFreeMic()
    _voiceState.value = VoiceState.OFF
}

// Speak a reply (markdown stripped, same as the phone): with the server's Kokoro
// voice when the toggle is on and the server offers TTS (the audio streams back
// and plays via Web Audio), else the browser's SpeechSynthesis. A lightweight
// poll flips `speaking` off once every engine and in-flight request drains, so
// the SpeakingBar and its stop button track real playback either way.
internal fun WebAppController.speak(text: String) = speak(text, prefs.ttsVoice)

internal fun WebAppController.speak(text: String, voice: String) {
    if (_audioOutput.value == AudioOutput.MUTE) return
    val spoken = Markdown.toSpeech(text)
    if (spoken.isBlank()) return
    if (prefs.serverTts && _serverTtsAvailable.value && _connected.value) {
        val id = "s${++speakSeq}"
        speakTexts[id] = spoken
        // Runaway guard; the server refuses past 32 queued anyway.
        while (speakTexts.size > 64) speakTexts.remove(speakTexts.keys.first())
        client?.send(Outbound.speak(id, spoken, voice = voice, format = "mp3"))
    } else {
        speakText(spoken)
    }
    _speaking.value = true
    if (speakWatch?.isActive != true) {
        speakWatch = scope.launch {
            while (speakTexts.isNotEmpty() || speechActive() || serverSpeechActive()) delay(250)
            _speaking.value = false
        }
    }
}

/** speak_audio: the next binary frames are this utterance's audio. Anything we
 *  didn't ask for (or a codec we can't decode) is dropped and falls back on
 *  its speak_end. */
internal fun WebAppController.onSpeakAudio(msg: ServerMsg.SpeakAudio) {
    speakStreamId = msg.id
    speakStreamLive = msg.codec == "mp3" && speakTexts.containsKey(msg.id)
    if (speakStreamLive) serverSpeakBegin()
}

/** A server→client binary frame — always speak audio (the only binary the
 *  server sends; ordered on the same socket as its speak_audio header). */
internal fun WebAppController.onSpeakFrame(data: ByteArray) {
    if (speakStreamLive) serverSpeakChunk(Base64.encode(data))
}

internal fun WebAppController.onSpeakEnd(msg: ServerMsg.SpeakEnd) {
    val wasLive = speakStreamLive && speakStreamId == msg.id
    if (speakStreamId == msg.id) {
        speakStreamId = null
        speakStreamLive = false
    }
    val text = speakTexts.remove(msg.id)
    if (wasLive) serverSpeakEnd() // decode the clip and queue it for playback
    // Refused (tts disabled / queue full / synthesis failed) → browser voice.
    // A stream that died part-way (wasLive) already queued partial audio;
    // don't also replay the whole utterance over it.
    if (msg.error.isNotEmpty() && text != null && !wasLive) speakText(text)
}

/** Forget all in-flight server speaks and silence their playback (barge-in,
 *  mute, disconnect). Frames still arriving for a cancelled utterance are
 *  dropped until its speak_end passes. */
internal fun WebAppController.cancelServerSpeech() {
    // speak_stop tells the server to drop its queue and abort the in-flight
    // synthesis too (moot when disconnected — the outbox just drops it).
    if (speakTexts.isNotEmpty() || speakStreamLive) client?.send(Outbound.speakStop())
    speakTexts.clear()
    speakStreamLive = false
    cancelServerSpeechPlayback()
}

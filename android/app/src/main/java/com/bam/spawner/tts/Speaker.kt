package com.bam.spawner.tts

import android.content.Context
import android.media.AudioAttributes
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import java.util.Locale
import java.util.concurrent.atomic.AtomicInteger

/**
 * Wraps Android TextToSpeech to read the server's `say`/`output` messages aloud.
 * Speech before init completes is queued.
 *
 * For the hands-free path: TTS is routed through VOICE_COMMUNICATION audio
 * attributes so the platform echo canceler can cancel it from the mic (the
 * default MUSIC stream often isn't cancelled), and the onSpeakingChanged callback
 * tracks playback so the recorder can raise its VAD bar / gate barge-in while
 * Claude is talking.
 */
class Speaker(context: Context) {
    private var ready = false
    private val pending = ArrayDeque<String>()
    private lateinit var tts: TextToSpeech

    // Number of utterances started but not yet finished/stopped.
    private val outstanding = AtomicInteger(0)
    @Volatile private var speakingCb: ((Boolean) -> Unit)? = null

    /** Notified when speaking starts (true) and stops (false). */
    fun onSpeakingChanged(cb: (Boolean) -> Unit) { speakingCb = cb }

    init {
        tts = TextToSpeech(context.applicationContext) { status ->
            if (status == TextToSpeech.SUCCESS) {
                tts.language = Locale.US
                tts.setAudioAttributes(
                    AudioAttributes.Builder()
                        .setUsage(AudioAttributes.USAGE_VOICE_COMMUNICATION)
                        .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build(),
                )
                tts.setOnUtteranceProgressListener(object : UtteranceProgressListener() {
                    override fun onStart(id: String?) { bump(1) }
                    override fun onDone(id: String?) { bump(-1) }
                    @Suppress("OVERRIDE_DEPRECATION")
                    override fun onError(id: String?) { bump(-1) }
                    override fun onError(id: String?, code: Int) { bump(-1) }
                    override fun onStop(id: String?, interrupted: Boolean) { bump(-1) }
                })
                ready = true
                while (pending.isNotEmpty()) speakNow(pending.removeFirst())
            }
        }
    }

    private fun bump(delta: Int) {
        var n = outstanding.addAndGet(delta)
        if (n < 0) { outstanding.set(0); n = 0 }
        speakingCb?.invoke(n > 0)
    }

    fun speak(text: String) {
        if (text.isBlank()) return
        if (ready) speakNow(text) else pending.addLast(text)
    }

    private fun speakNow(text: String) {
        tts.speak(text, TextToSpeech.QUEUE_ADD, null, text.hashCode().toString() + "-" + System.nanoTime())
    }

    /** Interrupt any in-progress and queued speech (barge-in). */
    fun stop() {
        pending.clear()
        if (::tts.isInitialized) tts.stop()
        outstanding.set(0)
        speakingCb?.invoke(false)
    }

    fun shutdown() {
        tts.stop()
        tts.shutdown()
    }
}

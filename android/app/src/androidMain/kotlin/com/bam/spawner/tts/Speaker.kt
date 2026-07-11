package com.bam.spawner.tts

import android.content.Context
import android.media.AudioAttributes
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioTrack
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import java.util.Locale
import java.util.concurrent.atomic.AtomicInteger
import kotlin.math.PI
import kotlin.math.cos
import kotlin.math.sin

/**
 * Wraps Android TextToSpeech to read the server's `say`/`output` messages aloud.
 * Speech before init completes is queued.
 *
 * Audio routing is mode-dependent (see [setCommMode]): normally it plays on the
 * MEDIA stream (regular loudspeaker, media volume). In hands-free it switches to
 * VOICE_COMMUNICATION so the platform echo canceller can cancel it from the open
 * mic — which is what keeps voice barge-in ("hey buddy stop") working while it's
 * talking. onSpeakingChanged tracks playback for the recorder + UI.
 */
class Speaker(context: Context) {
    private var ready = false
    private val pending = ArrayDeque<String>()
    private lateinit var tts: TextToSpeech
    @Volatile private var commMode = false // true = VOICE_COMMUNICATION (hands-free), false = MEDIA
    @Volatile private var muted = false    // when true, speak() is a no-op (Mute output)

    /** Suppress (or resume) all TTS. Muting also stops anything in progress. */
    fun setMuted(on: Boolean) {
        muted = on
        if (on) stop()
    }

    // Number of utterances started but not yet finished/stopped.
    private val outstanding = AtomicInteger(0)
    @Volatile private var speakingCb: ((Boolean) -> Unit)? = null

    /** Notified when speaking starts (true) and stops (false). */
    fun onSpeakingChanged(cb: (Boolean) -> Unit) { speakingCb = cb }

    /** Route TTS through communication audio (echo-cancelled, for hands-free) vs
     *  the regular media speaker. Applies to subsequent utterances. */
    fun setCommMode(on: Boolean) {
        commMode = on
        if (ready) applyAudioAttributes()
    }

    private fun applyAudioAttributes() {
        val usage = if (commMode) AudioAttributes.USAGE_VOICE_COMMUNICATION else AudioAttributes.USAGE_MEDIA
        tts.setAudioAttributes(
            AudioAttributes.Builder()
                .setUsage(usage)
                .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                .build(),
        )
    }

    init {
        tts = TextToSpeech(context.applicationContext) { status ->
            if (status == TextToSpeech.SUCCESS) {
                tts.language = Locale.US
                applyAudioAttributes()
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
        if (muted || text.isBlank()) return
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

    // The warm-beep waveform (PCM16 mono), synthesized once and replayed. It is a
    // low sine with a raised-cosine (Hann) envelope over the whole tone, plus a
    // touch of second harmonic for warmth — the smooth in/out means no click, so
    // it reads as a round "still working…" cue rather than a sharp alert, and
    // stays distinct from Android notification chimes.
    private val beepPcm: ByteArray by lazy { buildBeep() }
    @Volatile private var lastBeepNs = 0L

    /**
     * Play the soft warm beep in place of speaking an intermediate step (used by
     * summary-only mode). No-op when muted. Throttled so a burst of streamed
     * segments can't machine-gun it. Routed like TTS: echo-cancelled comms audio
     * in hands-free (so the open mic cancels it), the media speaker otherwise.
     */
    fun beep() {
        if (muted) return
        val now = System.nanoTime()
        if (now - lastBeepNs < BEEP_THROTTLE_MS * 1_000_000L) return
        lastBeepNs = now
        val usage = if (commMode) AudioAttributes.USAGE_VOICE_COMMUNICATION else AudioAttributes.USAGE_MEDIA
        val track = try {
            AudioTrack(
                AudioAttributes.Builder()
                    .setUsage(usage)
                    .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
                    .build(),
                AudioFormat.Builder()
                    .setSampleRate(BEEP_RATE)
                    .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                    .setChannelMask(AudioFormat.CHANNEL_OUT_MONO)
                    .build(),
                beepPcm.size, AudioTrack.MODE_STATIC, AudioManager.AUDIO_SESSION_ID_GENERATE,
            )
        } catch (_: Exception) {
            return
        }
        track.write(beepPcm, 0, beepPcm.size)
        track.play()
        // Release after it has finished (short daemon; the tone is ~200ms).
        Thread {
            try { Thread.sleep(BEEP_MS + 120L) } catch (_: InterruptedException) {}
            runCatching { track.stop() }
            runCatching { track.release() }
        }.apply { isDaemon = true }.start()
    }

    private fun buildBeep(): ByteArray {
        val n = (BEEP_RATE * BEEP_MS / 1000L).toInt()
        val buf = ByteArray(n * 2)
        val w = 2.0 * PI * BEEP_FREQ / BEEP_RATE
        for (i in 0 until n) {
            val env = 0.5 * (1.0 - cos(2.0 * PI * i / (n - 1))) // raised-cosine: smooth in and out
            val s = (sin(w * i) + 0.15 * sin(2.0 * w * i)) / 1.15 // low sine + gentle 2nd harmonic
            val v = (s * env * BEEP_AMP * Short.MAX_VALUE).toInt()
                .coerceIn(Short.MIN_VALUE.toInt(), Short.MAX_VALUE.toInt())
            buf[i * 2] = (v and 0xff).toByte()
            buf[i * 2 + 1] = ((v shr 8) and 0xff).toByte()
        }
        return buf
    }

    fun shutdown() {
        tts.stop()
        tts.shutdown()
    }
}

private const val BEEP_RATE = 44100
private const val BEEP_MS = 200L
private const val BEEP_FREQ = 420.0    // low and round — warm, not shrill
private const val BEEP_AMP = 0.30      // soft
// Just over the tone length: two distinct on-screen messages that land close
// together (e.g. a subagent finishing dumps a few in a row) each still get their
// own beep — the throttle only coalesces beeps that would physically overlap,
// rather than silently eating messages a few hundred ms apart (the old 700ms did).
private const val BEEP_THROTTLE_MS = 220L

package com.bam.spawner.tts

import android.content.Context
import android.media.AudioAttributes
import android.media.AudioFormat
import android.media.AudioManager
import android.media.AudioTrack
import android.speech.tts.TextToSpeech
import android.speech.tts.UtteranceProgressListener
import java.util.Locale
import java.util.concurrent.LinkedBlockingQueue
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
        clearBeeps()
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
    // Serialized beep pipeline: every beep() enqueues a tone that a single worker
    // plays back-to-back, so a burst of streamed segments each gets its own audible
    // cue instead of the loudest one swallowing the rest. The old design *dropped*
    // any beep landing within a throttle window, which is exactly why messages that
    // arrive close together (a subagent dumping several, or fast streamed chunks)
    // went unbeeped. Bounded so a runaway burst can't beep for many seconds.
    private val beepQueue = LinkedBlockingQueue<Unit>()
    private val beepPending = AtomicInteger(0)
    @Volatile private var beepWorker: Thread? = null
    // The worker owns one long-lived AudioTrack and replays it, which keeps the
    // audio route warm — a freshly-created track per beep let the platform swallow
    // the first tone after an idle route (the "missing on the first message" symptom).
    private var beepTrack: AudioTrack? = null
    private var beepTrackUsage = -1

    /**
     * Play the soft warm beep in place of speaking an intermediate step (used by
     * summary-only mode). No-op when muted. Serialized (not throttled) so every
     * message gets a beep — a burst plays as distinct back-to-back tones. Routed
     * like TTS: echo-cancelled comms audio in hands-free (so the open mic cancels
     * it), the media speaker otherwise.
     */
    fun beep() {
        if (muted) return
        // Cap the backlog so a huge burst doesn't machine-gun for many seconds; past
        // the cap the extra messages are already represented by the queued beeps.
        if (beepPending.get() >= MAX_PENDING_BEEPS) return
        beepPending.incrementAndGet()
        ensureBeepWorker()
        beepQueue.offer(Unit)
    }

    @Synchronized
    private fun ensureBeepWorker() {
        if (beepWorker?.isAlive == true) return
        beepWorker = Thread {
            while (true) {
                try {
                    beepQueue.take()
                } catch (_: InterruptedException) {
                    break
                }
                if (!muted) runCatching { playOneBeep() }
                beepPending.updateAndGet { if (it > 0) it - 1 else 0 }
                try {
                    Thread.sleep(BEEP_GAP_MS) // brief gap so back-to-back tones stay distinct
                } catch (_: InterruptedException) {
                    break
                }
            }
        }.apply { isDaemon = true; name = "warm-beep"; start() }
    }

    // Runs only on the single beep worker thread, so no locking is needed for the
    // reused track. Recreates it if the audio route (media vs comms) changed.
    private fun playOneBeep() {
        val usage = if (commMode) AudioAttributes.USAGE_VOICE_COMMUNICATION else AudioAttributes.USAGE_MEDIA
        var track = beepTrack
        if (track == null || beepTrackUsage != usage) {
            runCatching { track?.release() }
            track = try {
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
                ).also { it.write(beepPcm, 0, beepPcm.size) }
            } catch (_: Exception) {
                null
            }
            beepTrack = track
            beepTrackUsage = if (track != null) usage else -1
        }
        val t = track ?: return
        runCatching {
            t.stop() // static track must be stopped before it can be rewound + replayed
            t.reloadStaticData() // rewind the static buffer to the start
            t.play()
        }
        try { Thread.sleep(BEEP_MS + 40L) } catch (_: InterruptedException) {} // let the tone finish before the next
    }

    /** Drop any queued/pending beeps (used on barge-in and mute). */
    private fun clearBeeps() {
        beepQueue.clear()
        beepPending.set(0)
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
        clearBeeps()
        beepWorker?.interrupt()
        runCatching { beepTrack?.release() }
        beepTrack = null
        tts.stop()
        tts.shutdown()
    }
}

private const val BEEP_RATE = 44100
private const val BEEP_MS = 200L
private const val BEEP_FREQ = 420.0    // low and round — warm, not shrill
private const val BEEP_AMP = 0.30      // soft
private const val BEEP_GAP_MS = 70L    // silence between queued back-to-back tones so they stay distinct
private const val MAX_PENDING_BEEPS = 12 // backlog cap so a runaway burst can't beep for many seconds

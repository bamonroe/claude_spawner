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

    /** Whether TTS is muted (Mute output) — server-TTS callers check before asking
     *  the server to synthesize anything for this client. */
    fun isMuted() = muted

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
        streamStop()
        if (::tts.isInitialized) tts.stop()
        outstanding.set(0)
        speakingCb?.invoke(false)
    }

    // --- Server-TTS stream playback (the Kokoro epic, see TODO.md) -------------
    //
    // The server synthesizes speech and streams raw PCM (24 kHz s16le mono, the
    // `pcm` speak format) as binary WebSocket frames; this section plays them on
    // a MODE_STREAM AudioTrack. Like the beeps, all track access happens on one
    // dedicated worker thread fed by a queue — AudioTrack.write blocks until the
    // track buffer has room, which must never stall the network reader thread.
    // Routing matches spoken TTS: echo-cancelled comms audio in hands-free (so
    // voice barge-in works while it talks), the media speaker otherwise.

    private sealed interface StreamEvt {
        data object Begin : StreamEvt
        class Data(val bytes: ByteArray) : StreamEvt
        data object End : StreamEvt
    }

    private val streamQueue = LinkedBlockingQueue<StreamEvt>()
    @Volatile private var streamWorker: Thread? = null
    private var streamTrack: AudioTrack? = null // worker-thread-owned
    private var streamTrackUsage = -1
    @Volatile private var streamDirty = false // barge-in flushed the track: rebuild on next Begin
    @Volatile private var streamBumped = false // this stream counted itself in `outstanding`
    private var streamFramesWritten = 0L

    /** An utterance's audio stream is starting (speak_audio arrived). */
    fun streamBegin() {
        if (muted) return
        ensureStreamWorker()
        streamQueue.offer(StreamEvt.Begin)
    }

    /** One binary frame of the current utterance's PCM. */
    fun streamWrite(data: ByteArray) {
        if (muted) return
        streamQueue.offer(StreamEvt.Data(data))
    }

    /** The current utterance's stream closed cleanly (speak_end). */
    fun streamEnd() {
        streamQueue.offer(StreamEvt.End)
    }

    /** Silence server-TTS playback immediately (barge-in / mute / disconnect) and
     *  drop whatever is queued. The caller stops forwarding the rest of the
     *  in-flight stream; the next streamBegin rebuilds the track. */
    fun streamStop() {
        streamQueue.clear()
        streamDirty = true
        // pause+flush from another thread also unblocks a worker stuck in write().
        runCatching { streamTrack?.pause(); streamTrack?.flush() }
        if (streamBumped) { streamBumped = false; bump(-1) }
    }

    @Synchronized
    private fun ensureStreamWorker() {
        if (streamWorker?.isAlive == true) return
        streamWorker = Thread {
            while (true) {
                val evt = try {
                    streamQueue.take()
                } catch (_: InterruptedException) {
                    break
                }
                runCatching {
                    when (evt) {
                        is StreamEvt.Begin -> streamBeginOnWorker()
                        is StreamEvt.Data -> {
                            val t = streamTrack
                            if (t != null && !streamDirty) {
                                val n = t.write(evt.bytes, 0, evt.bytes.size)
                                if (n > 0) streamFramesWritten += n / 2 // 16-bit mono: 2 bytes/frame
                            }
                        }
                        is StreamEvt.End -> streamEndOnWorker()
                    }
                }
            }
        }.apply { isDaemon = true; name = "tts-stream"; start() }
    }

    // Worker-only: (re)build the track when first used, when the route (media vs
    // comms) changed, or after a barge-in flush, then start playback.
    private fun streamBeginOnWorker() {
        val usage = if (commMode) AudioAttributes.USAGE_VOICE_COMMUNICATION else AudioAttributes.USAGE_MEDIA
        if (streamTrack == null || streamTrackUsage != usage || streamDirty) {
            runCatching { streamTrack?.release() }
            streamTrack = try {
                val minBuf = AudioTrack.getMinBufferSize(
                    STREAM_RATE, AudioFormat.CHANNEL_OUT_MONO, AudioFormat.ENCODING_PCM_16BIT,
                )
                AudioTrack(
                    AudioAttributes.Builder()
                        .setUsage(usage)
                        .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build(),
                    AudioFormat.Builder()
                        .setSampleRate(STREAM_RATE)
                        .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                        .setChannelMask(AudioFormat.CHANNEL_OUT_MONO)
                        .build(),
                    maxOf(minBuf, STREAM_RATE) /* ≥ half a second buffered */,
                    AudioTrack.MODE_STREAM, AudioManager.AUDIO_SESSION_ID_GENERATE,
                )
            } catch (_: Exception) {
                null
            }
            streamTrackUsage = if (streamTrack != null) usage else -1
            streamDirty = false
        }
        streamFramesWritten = 0
        val t = streamTrack ?: return
        runCatching { t.play() }
        if (!streamBumped) { streamBumped = true; bump(1) } // drives onSpeakingChanged like spoken TTS
    }

    // Worker-only: let the buffered tail play out (so the speaking indicator and
    // the hands-free recorder gating stay honest), then stop the track.
    private fun streamEndOnWorker() {
        val t = streamTrack
        if (t != null && !streamDirty) {
            val deadline = System.nanoTime() +
                ((streamFramesWritten * 1000L / STREAM_RATE) + 1000L) * 1_000_000L
            while (System.nanoTime() < deadline) {
                val head = runCatching { t.playbackHeadPosition.toLong() and 0xffffffffL }.getOrNull() ?: break
                if (head >= streamFramesWritten || t.playState != AudioTrack.PLAYSTATE_PLAYING) break
                try { Thread.sleep(40) } catch (_: InterruptedException) { break }
            }
            runCatching { t.stop() }
        }
        if (streamBumped) { streamBumped = false; bump(-1) }
    }

    // The warm-beep waveform (PCM16 mono), synthesized once and replayed. It is a
    // low sine with a raised-cosine (Hann) envelope over the whole tone, plus a
    // touch of second harmonic for warmth — the smooth in/out means no click, so
    // it reads as a round "still working…" cue rather than a sharp alert, and
    // stays distinct from Android notification chimes.
    private val beepPcm: ByteArray by lazy { buildBeep() }
    // The acknowledgment chirp: a short two-note rising figure ("ba-dip"), played
    // once when the server echoes back a recognized utterance — the cue that your
    // dictation was heard and dispatched to the session. Deliberately distinct from
    // the warm beep (a single low tone for Claude's activity) so "you were heard"
    // never sounds like "here's the reply". Plays on the same warm-track worker.
    private val chirpPcm: ByteArray by lazy { buildChirp() }
    private enum class Tone { BEEP, CHIRP }
    // Coalescing "still working…" cue: a beep is swallowed while one is already
    // playing, so a burst of messages doesn't machine-gun — it's an indicator that
    // things are happening, not a per-message count. But a beep that arrives once
    // the current tone has finished always plays, so sustained activity gives a
    // steady stream of tones rather than long silent gaps.
    //
    // The tone plays on one long-lived, reused AudioTrack driven by a single worker
    // thread. That's the actual fix for the reported 20-second silent gaps: the old
    // code built (and released) a fresh AudioTrack for every beep, and the platform
    // routinely swallowed those cold-route plays — so beeps were requested but never
    // sounded. Keeping the track warm makes each replay reliably audible.
    private val beepQueue = LinkedBlockingQueue<Tone>()
    @Volatile private var beepWorker: Thread? = null
    @Volatile private var lastBeepNs = 0L
    private var beepTrack: AudioTrack? = null
    private var beepTrackUsage = -1
    // The chirp gets its own warm static track so its buffer never clobbers the beep's.
    private var chirpTrack: AudioTrack? = null
    private var chirpTrackUsage = -1

    /**
     * Play the soft warm beep in place of speaking an intermediate step (used by
     * summary-only mode). No-op when muted. Coalesced: swallowed while a tone is
     * already playing, so bursts read as "activity" rather than a beep per message.
     * Routed like TTS: echo-cancelled comms audio in hands-free (so the open mic
     * cancels it), the media speaker otherwise.
     */
    fun beep() {
        if (muted) return
        // Swallow a beep that lands while the previous tone is still sounding; the
        // window is the tone length plus a short gap, so the worker never backs up
        // (at most one tone is ever queued) and back-to-back activity still beeps
        // steadily as each window elapses.
        val now = System.nanoTime()
        if (now - lastBeepNs < BEEP_COALESCE_MS * 1_000_000L) return
        lastBeepNs = now
        ensureBeepWorker()
        beepQueue.offer(Tone.BEEP)
    }

    /**
     * Play the acknowledgment chirp: fired once when the server echoes a recognized
     * utterance, so you hear that your dictation was heard and dispatched even though
     * Claude hasn't replied yet. No-op when muted. Not coalesced — it's one event per
     * turn, and it rides the same warm-track worker as [beep] for reliable playback.
     */
    fun chirp() {
        if (muted) return
        ensureBeepWorker()
        beepQueue.offer(Tone.CHIRP)
    }

    @Synchronized
    private fun ensureBeepWorker() {
        if (beepWorker?.isAlive == true) return
        beepWorker = Thread {
            while (true) {
                val tone = try {
                    beepQueue.take()
                } catch (_: InterruptedException) {
                    break
                }
                if (!muted) runCatching { playTone(tone) }
            }
        }.apply { isDaemon = true; name = "warm-beep"; start() }
    }

    // Runs only on the single beep worker thread, so no locking is needed for the
    // reused tracks. Recreates a track if the audio route (media vs comms) changed.
    // Beep and chirp keep separate warm tracks so their static buffers don't clash.
    private fun playTone(tone: Tone) {
        val usage = if (commMode) AudioAttributes.USAGE_VOICE_COMMUNICATION else AudioAttributes.USAGE_MEDIA
        val pcm = if (tone == Tone.CHIRP) chirpPcm else beepPcm
        var track = if (tone == Tone.CHIRP) chirpTrack else beepTrack
        val trackUsage = if (tone == Tone.CHIRP) chirpTrackUsage else beepTrackUsage
        if (track == null || trackUsage != usage) {
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
                    pcm.size, AudioTrack.MODE_STATIC, AudioManager.AUDIO_SESSION_ID_GENERATE,
                ).also { it.write(pcm, 0, pcm.size) }
            } catch (_: Exception) {
                null
            }
            if (tone == Tone.CHIRP) {
                chirpTrack = track
                chirpTrackUsage = if (track != null) usage else -1
            } else {
                beepTrack = track
                beepTrackUsage = if (track != null) usage else -1
            }
        }
        val t = track ?: return
        runCatching {
            t.stop() // static track must be stopped before it can be rewound + replayed
            t.reloadStaticData() // rewind the static buffer to the start
            t.play()
        }
        val ms = if (tone == Tone.CHIRP) CHIRP_TOTAL_MS else BEEP_MS
        try { Thread.sleep(ms + 40L) } catch (_: InterruptedException) {} // let the tone finish before the next
    }

    /** Drop any queued beeps (used on barge-in and mute). */
    private fun clearBeeps() {
        beepQueue.clear()
        lastBeepNs = 0L
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

    // Two short rising notes with a brief gap between them — a "ba-dip" that reads as
    // "got it". Each note has a raised-cosine envelope (no click); the gap is silence.
    // Rising pitch and the two-note shape keep it clearly apart from the single low beep.
    private fun buildChirp(): ByteArray {
        val segments = listOf(
            Triple(CHIRP_FREQ_LO, CHIRP_NOTE_MS, true),  // low note
            Triple(0.0, CHIRP_GAP_MS, false),            // silent gap
            Triple(CHIRP_FREQ_HI, CHIRP_NOTE_MS, true),  // higher note
        )
        val total = segments.sumOf { (BEEP_RATE * it.second / 1000L).toInt() }
        val buf = ByteArray(total * 2)
        var idx = 0
        for ((freq, ms, sound) in segments) {
            val n = (BEEP_RATE * ms / 1000L).toInt()
            val w = 2.0 * PI * freq / BEEP_RATE
            for (i in 0 until n) {
                var v = 0
                if (sound) {
                    val env = 0.5 * (1.0 - cos(2.0 * PI * i / (n - 1))) // smooth in/out
                    v = (sin(w * i) * env * CHIRP_AMP * Short.MAX_VALUE).toInt()
                        .coerceIn(Short.MIN_VALUE.toInt(), Short.MAX_VALUE.toInt())
                }
                buf[idx * 2] = (v and 0xff).toByte()
                buf[idx * 2 + 1] = ((v shr 8) and 0xff).toByte()
                idx++
            }
        }
        return buf
    }

    fun shutdown() {
        clearBeeps()
        beepWorker?.interrupt()
        streamStop()
        streamWorker?.interrupt()
        runCatching { beepTrack?.release() }
        runCatching { chirpTrack?.release() }
        runCatching { streamTrack?.release() }
        beepTrack = null
        chirpTrack = null
        streamTrack = null
        tts.stop()
        tts.shutdown()
    }
}

// Kokoro's `pcm` speak format: raw 24 kHz 16-bit little-endian mono (docs/protocol.md).
private const val STREAM_RATE = 24000
private const val BEEP_RATE = 44100
private const val BEEP_MS = 200L
private const val BEEP_FREQ = 420.0    // low and round — warm, not shrill
private const val BEEP_AMP = 0.30      // soft
// A beep is swallowed while another is within this window of starting — the tone
// length plus a short gap. So a burst coalesces to one "activity" tone, but once a
// tone finishes the next message beeps again (steady stream under sustained work).
private const val BEEP_COALESCE_MS = 260L
// The acknowledgment chirp: two short rising notes with a silent gap between them.
private const val CHIRP_NOTE_MS = 75L
private const val CHIRP_GAP_MS = 30L
private const val CHIRP_TOTAL_MS = CHIRP_NOTE_MS * 2 + CHIRP_GAP_MS
private const val CHIRP_FREQ_LO = 660.0 // brighter than the beep, clearly a different cue
private const val CHIRP_FREQ_HI = 880.0 // a rising step up — reads as "got it"
private const val CHIRP_AMP = 0.25      // a touch softer than the beep

package com.bam.spawner.audio

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.media.audiofx.AcousticEchoCanceler
import android.media.audiofx.NoiseSuppressor
import android.util.Log
import java.io.File
import kotlin.concurrent.thread
import com.bam.spawner.net.Codecs

/**
 * Hands-free capture: one continuous AudioRecord loop segments speech into
 * per-utterance Ogg/Opus clips using [Endpointer] (VAD). Silence is never
 * encoded or sent — only detected speech, which is what keeps cellular data low.
 *
 * A ~320 ms pre-roll ring buffer is flushed into the encoder at speech onset so
 * the first word ("hey …") isn't clipped (VAD declares onset slightly late).
 * Uses VOICE_COMMUNICATION + AcousticEchoCanceler so the phone's own TTS doesn't
 * self-trigger the mic during barge-in; while [playbackActive] the VAD energy
 * bar is raised to reject residual echo.
 *
 * Callbacks fire on the capture thread: [onSpeechStart] at onset (barge-in / UI),
 * [onUtterance] with the finished clip bytes at end-of-speech.
 */
/** User-tunable voice-activity-detection dials (persisted in SettingsStore). */
data class VadConfig(
    val rmsThreshold: Double = 500.0,
    val onsetMs: Int = 120,
    val silenceMs: Int = 800,
    // When true, the energy bar tracks the room's ambient noise floor instead of
    // sitting at a fixed [rmsThreshold]; see Endpointer.
    val adaptive: Boolean = true,
)

class HandsFreeRecorder(
    private val context: Context,
    private val vad: VadConfig,
    private val onSpeechStart: () -> Unit,
    private val onUtterance: (ByteArray) -> Unit,
    private val onLevel: ((Double) -> Unit)? = null,
    // Capture source + echo cancellation. Defaults suit speaker output (comm audio
    // so the platform AEC can cancel our own TTS for barge-in). With headphones the
    // controller passes VOICE_RECOGNITION + aec=false, which keeps the system out of
    // call mode so other apps' media isn't ducked (our TTS is in the user's ears, so
    // there's nothing to echo-cancel).
    private val audioSource: Int = MediaRecorder.AudioSource.VOICE_COMMUNICATION,
    private val enableAec: Boolean = true,
    // Whether to run the platform NoiseSuppressor. Defaults to follow [enableAec]
    // (the AEC path always suppressed too); the headset/media path can force it on
    // independently so ambient noise is filtered even with the echo canceller off.
    private val enableNs: Boolean = enableAec,
    // When > 0 the recorder ignores the VAD entirely and captures one fixed-length
    // clip of this many milliseconds, then emits it via [onUtterance]. Used for the
    // "stay silent" training prompt, where there is no speech to end-point on so the
    // normal VAD-gated path would never terminate.
    private val fixedMs: Int = 0,
) {
    companion object {
        const val CODEC = Codecs.OGG_OPUS
        private const val TAG = "SpawnerMic"
        private const val SAMPLE_RATE = OpusOggEncoder.SAMPLE_RATE
        private const val FRAME_MS = 20
        private const val FRAME_BYTES = SAMPLE_RATE * 2 * FRAME_MS / 1000 // 640
        private const val PREROLL_FRAMES = 16 // ~320 ms
    }

    private val rmsIdle = vad.rmsThreshold
    private val rmsPlayback = vad.rmsThreshold * 3 // raised bar while TTS is speaking (echo)

    @Volatile private var running = false
    /** Set true by the controller while TTS is speaking (raises the VAD bar). */
    @Volatile var playbackActive = false
    private var worker: Thread? = null
    private var aec: AcousticEchoCanceler? = null
    private var ns: NoiseSuppressor? = null

    @SuppressLint("MissingPermission") // caller holds RECORD_AUDIO
    fun start(): Boolean {
        if (running) return false
        val minBuf = AudioRecord.getMinBufferSize(
            SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
        )
        if (minBuf <= 0) {
            Log.e(TAG, "bad min buffer $minBuf"); return false
        }
        val record = try {
            AudioRecord(
                audioSource, SAMPLE_RATE,
                AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, maxOf(minBuf, FRAME_BYTES * PREROLL_FRAMES),
            )
        } catch (e: Exception) {
            Log.e(TAG, "AudioRecord ctor failed", e); return false
        }
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            Log.e(TAG, "AudioRecord not initialized"); record.release(); return false
        }
        enableEffects(record.audioSessionId)
        running = true
        record.startRecording()
        worker = thread(name = "handsfree") { captureLoop(record) }
        return true
    }

    fun stop() {
        running = false
        worker?.join(1500)
        worker = null
        runCatching { aec?.release() }; aec = null
        runCatching { ns?.release() }; ns = null
    }

    private fun enableEffects(sessionId: Int) {
        runCatching {
            if (enableAec && AcousticEchoCanceler.isAvailable()) {
                aec = AcousticEchoCanceler.create(sessionId)?.also { it.enabled = true }
            }
            // Noise suppression is independent of echo cancellation: by default it
            // follows the AEC path, but the headset/media path can opt in via
            // [enableNs]. Left off there by default because the suppressor is tuned
            // for a near mic and can treat far-field voice as noise and attenuate it.
            if (enableNs && NoiseSuppressor.isAvailable()) {
                ns = NoiseSuppressor.create(sessionId)?.also { it.enabled = true }
            }
        }
    }

    /**
     * Fixed-length capture: no VAD, no onset gate — encode every frame for [fixedMs]
     * then emit the clip. Used for the "stay silent" prompt so a quiet take actually
     * terminates and produces a clip.
     */
    private fun captureFixed(record: AudioRecord) {
        val frame = ByteArray(FRAME_BYTES)
        val file = File(context.cacheDir, "handsfree.ogg")
        val enc = OpusOggEncoder()
        if (!enc.start(file)) {
            Log.e(TAG, "fixed capture: encoder start failed")
            runCatching { record.stop() }; runCatching { record.release() }; return
        }
        onSpeechStart()
        var elapsedMs = 0
        try {
            while (running && elapsedMs < fixedMs) {
                val n = record.read(frame, 0, FRAME_BYTES)
                if (n <= 0) continue
                onLevel?.invoke(pcm16Rms(frame, n))
                enc.feed(frame, n)
                elapsedMs += FRAME_MS
            }
        } catch (e: Exception) {
            Log.e(TAG, "fixed capture failed", e)
        } finally {
            running = false
            val bytes = enc.finish()
            runCatching { record.stop() }; runCatching { record.release() }
            if (bytes != null && bytes.isNotEmpty()) onUtterance(bytes)
        }
    }

    private fun captureLoop(record: AudioRecord) {
        if (fixedMs > 0) { captureFixed(record); return }
        val frame = ByteArray(FRAME_BYTES)
        val ring = ArrayDeque<ByteArray>(PREROLL_FRAMES + 1)
        var encoder: OpusOggEncoder? = null
        var startReq = false
        var endReq = false
        val endpointer = Endpointer(
            silenceMs = vad.silenceMs,
            rmsThreshold = vad.rmsThreshold,
            onsetMs = vad.onsetMs,
            adaptive = vad.adaptive,
            onStart = { startReq = true },
            onEnd = { endReq = true },
        )
        val file = File(context.cacheDir, "handsfree.ogg")

        try {
            while (running) {
                val n = record.read(frame, 0, FRAME_BYTES)
                if (n <= 0) continue
                onLevel?.invoke(pcm16Rms(frame, n)) // feed the live level meter
                val chunk = if (n == FRAME_BYTES) frame.copyOf() else frame.copyOf(n)

                if (encoder == null) {
                    ring.addLast(chunk)
                    while (ring.size > PREROLL_FRAMES) ring.removeFirst()
                }
                endpointer.setThreshold(if (playbackActive) rmsPlayback else rmsIdle)
                endpointer.feed(chunk, FRAME_MS)

                when {
                    encoder == null && startReq -> {
                        startReq = false
                        val enc = OpusOggEncoder()
                        if (enc.start(file)) {
                            encoder = enc
                            for (f in ring) enc.feed(f, f.size) // pre-roll (incl. onset frame)
                            ring.clear()
                            onSpeechStart()
                        } else {
                            endpointer.reset() // couldn't start; drop this onset
                        }
                    }
                    encoder != null -> {
                        encoder.feed(chunk, n)
                        if (endReq) {
                            endReq = false
                            val bytes = encoder.finish()
                            encoder = null
                            endpointer.reset()
                            if (bytes != null && bytes.isNotEmpty()) onUtterance(bytes)
                        }
                    }
                }
            }
        } catch (e: Exception) {
            Log.e(TAG, "capture loop failed", e)
        } finally {
            encoder?.finish()?.let { if (it.isNotEmpty()) onUtterance(it) }
            runCatching { record.stop() }; runCatching { record.release() }
        }
    }
}

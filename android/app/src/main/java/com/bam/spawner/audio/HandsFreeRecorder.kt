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
)

class HandsFreeRecorder(
    private val context: Context,
    private val vad: VadConfig,
    private val onSpeechStart: () -> Unit,
    private val onUtterance: (ByteArray) -> Unit,
    private val onLevel: ((Double) -> Unit)? = null,
) {
    companion object {
        const val CODEC = "ogg_opus"
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
                MediaRecorder.AudioSource.VOICE_COMMUNICATION, SAMPLE_RATE,
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
            if (AcousticEchoCanceler.isAvailable()) {
                aec = AcousticEchoCanceler.create(sessionId)?.also { it.enabled = true }
            }
            if (NoiseSuppressor.isAvailable()) {
                ns = NoiseSuppressor.create(sessionId)?.also { it.enabled = true }
            }
        }
    }

    private fun captureLoop(record: AudioRecord) {
        val frame = ByteArray(FRAME_BYTES)
        val ring = ArrayDeque<ByteArray>(PREROLL_FRAMES + 1)
        var encoder: OpusOggEncoder? = null
        var startReq = false
        var endReq = false
        val endpointer = Endpointer(
            silenceMs = vad.silenceMs,
            rmsThreshold = vad.rmsThreshold,
            onsetMs = vad.onsetMs,
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

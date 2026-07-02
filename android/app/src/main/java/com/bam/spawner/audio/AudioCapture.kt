package com.bam.spawner.audio

import android.annotation.SuppressLint
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.util.Log
import kotlin.concurrent.thread

/**
 * Captures microphone audio as PCM16LE / 16 kHz / mono and streams it frame by
 * frame — the exact format the server wraps into a WAV (docs/protocol.md).
 * Requires the RECORD_AUDIO permission (the caller must have obtained it).
 */
class AudioCapture(private val onFrame: (ByteArray) -> Unit) {

    companion object {
        const val SAMPLE_RATE = 16000
        private const val FRAME_BYTES = 3200 // 100 ms @ 16 kHz mono PCM16
        private const val TAG = "SpawnerMic"
    }

    @Volatile private var running = false
    private var record: AudioRecord? = null

    @SuppressLint("MissingPermission") // caller guarantees RECORD_AUDIO
    fun start() {
        if (running) return
        val minBuf = AudioRecord.getMinBufferSize(
            SAMPLE_RATE,
            AudioFormat.CHANNEL_IN_MONO,
            AudioFormat.ENCODING_PCM_16BIT,
        )
        Log.i(TAG, "getMinBufferSize=$minBuf")
        if (minBuf <= 0) {
            Log.e(TAG, "invalid min buffer size ($minBuf); mic config unsupported")
            return
        }
        val bufSize = maxOf(minBuf, FRAME_BYTES * 2)

        // Try VOICE_RECOGNITION first (tuned for speech), fall back to MIC.
        val r = build(MediaRecorder.AudioSource.VOICE_RECOGNITION, bufSize)
            ?: build(MediaRecorder.AudioSource.MIC, bufSize)
        if (r == null) {
            Log.e(TAG, "AudioRecord failed to initialize on both sources")
            return
        }
        record = r
        running = true
        try {
            r.startRecording()
        } catch (e: IllegalStateException) {
            Log.e(TAG, "startRecording failed", e)
            running = false
            r.release(); record = null
            return
        }
        Log.i(TAG, "recording started, recordingState=${r.recordingState}")

        thread(name = "audio-capture") {
            val buf = ByteArray(FRAME_BYTES)
            var total = 0
            var firstLogged = false
            while (running) {
                val n = r.read(buf, 0, buf.size)
                if (n > 0) {
                    total += n
                    if (!firstLogged) { Log.i(TAG, "first read n=$n"); firstLogged = true }
                    onFrame(if (n == buf.size) buf.copyOf() else buf.copyOf(n))
                } else if (n < 0) {
                    Log.e(TAG, "AudioRecord.read error=$n; stopping")
                    break
                }
            }
            Log.i(TAG, "capture loop ended, total=$total bytes")
        }
    }

    @SuppressLint("MissingPermission")
    private fun build(source: Int, bufSize: Int): AudioRecord? {
        return try {
            val r = AudioRecord(
                source, SAMPLE_RATE,
                AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, bufSize,
            )
            if (r.state != AudioRecord.STATE_INITIALIZED) {
                Log.e(TAG, "source=$source not initialized (state=${r.state})")
                r.release()
                null
            } else {
                Log.i(TAG, "AudioRecord initialized on source=$source")
                r
            }
        } catch (e: Exception) {
            Log.e(TAG, "AudioRecord ctor failed on source=$source", e)
            null
        }
    }

    fun stop() {
        running = false
        record?.let {
            try {
                it.stop()
            } catch (_: IllegalStateException) {
            }
            it.release()
        }
        record = null
    }
}

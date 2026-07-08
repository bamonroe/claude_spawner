package com.bam.spawner.audio

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.util.Log
import java.io.File
import kotlin.concurrent.thread

/**
 * Push-to-talk recorder: captures one Ogg/Opus utterance while the button is
 * held (start → stopAndRead). Uses AudioRecord (VOICE_RECOGNITION, instant/
 * reliable) feeding the shared [OpusOggEncoder]. ~10x smaller than raw PCM.
 * The server decodes the Ogg/Opus (ffmpeg) for whisper.
 */
class OpusRecorder(private val context: Context) {
    companion object {
        const val CODEC = "ogg_opus"
        private const val TAG = "SpawnerMic"
        private const val SAMPLE_RATE = OpusOggEncoder.SAMPLE_RATE
    }

    @Volatile private var running = false
    private var worker: Thread? = null
    @Volatile private var result: ByteArray? = null

    @SuppressLint("MissingPermission") // caller holds RECORD_AUDIO
    fun start(): Boolean {
        if (running) return false
        val minBuf = AudioRecord.getMinBufferSize(
            SAMPLE_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
        )
        if (minBuf <= 0) {
            Log.e(TAG, "bad min buffer $minBuf")
            return false
        }
        val record = try {
            AudioRecord(
                MediaRecorder.AudioSource.VOICE_RECOGNITION, SAMPLE_RATE,
                AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, maxOf(minBuf, 4096),
            )
        } catch (e: Exception) {
            Log.e(TAG, "AudioRecord ctor failed", e); return false
        }
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            Log.e(TAG, "AudioRecord not initialized"); record.release(); return false
        }

        val encoder = OpusOggEncoder()
        if (!encoder.start(File(context.cacheDir, "utterance.ogg"))) {
            record.release(); return false
        }
        running = true
        result = null
        record.startRecording()

        worker = thread(name = "opus-enc") {
            val pcm = ByteArray(2048)
            try {
                while (running) {
                    val n = record.read(pcm, 0, pcm.size)
                    if (n > 0) encoder.feed(pcm, n)
                }
            } catch (e: Exception) {
                Log.e(TAG, "capture failed", e)
            } finally {
                result = encoder.finish()
                runCatching { record.stop() }; runCatching { record.release() }
            }
        }
        return true
    }

    /** Stops recording and returns the encoded Ogg/Opus bytes (null on failure). */
    fun stopAndRead(): ByteArray? {
        if (!running) return null
        running = false
        worker?.join(2000)
        worker = null
        return result
    }
}

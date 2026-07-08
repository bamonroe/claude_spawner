package com.bam.spawner.audio

import android.annotation.SuppressLint
import android.content.Context
import android.media.AudioFormat
import android.media.AudioRecord
import android.media.MediaRecorder
import android.util.Log
import kotlin.concurrent.thread
import kotlin.math.sqrt

/** RMS (~0..32768) of PCM16LE bytes — the same energy the VAD threshold compares against. */
fun pcm16Rms(frame: ByteArray, len: Int): Double {
    if (len < 2) return 0.0
    var sum = 0.0
    var i = 0
    val n = len - 1
    while (i < n) {
        val s = (frame[i].toInt() and 0xff) or (frame[i + 1].toInt() shl 8)
        sum += (s * s).toDouble()
        i += 2
    }
    return sqrt(sum / (len / 2))
}

/**
 * Reads the mic and reports the current input RMS level (via [onLevel]) for a
 * live meter — no audio is sent anywhere. Used on the Audio settings page when
 * hands-free isn't already running (which would otherwise feed the level itself).
 */
class LevelMeter(private val context: Context, private val onLevel: (Double) -> Unit) {
    @Volatile private var running = false
    private var worker: Thread? = null

    @SuppressLint("MissingPermission") // caller holds RECORD_AUDIO
    fun start(): Boolean {
        if (running) return false
        val sr = 16000
        val minBuf = AudioRecord.getMinBufferSize(sr, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT)
        if (minBuf <= 0) return false
        val record = try {
            AudioRecord(
                MediaRecorder.AudioSource.VOICE_COMMUNICATION, sr,
                AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT, maxOf(minBuf, 4096),
            )
        } catch (e: Exception) {
            Log.e("SpawnerMic", "meter ctor failed", e); return false
        }
        if (record.state != AudioRecord.STATE_INITIALIZED) {
            record.release(); return false
        }
        running = true
        record.startRecording()
        worker = thread(name = "level-meter") {
            val buf = ByteArray(1024) // ~32 ms
            try {
                while (running) {
                    val n = record.read(buf, 0, buf.size)
                    if (n > 0) onLevel(pcm16Rms(buf, n))
                }
            } catch (e: Exception) {
                Log.e("SpawnerMic", "meter loop failed", e)
            } finally {
                runCatching { record.stop() }; runCatching { record.release() }
            }
        }
        return true
    }

    fun stop() {
        running = false
        worker?.join(500)
        worker = null
        onLevel(0.0)
    }
}

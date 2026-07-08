package com.bam.spawner.audio

import android.media.MediaCodec
import android.media.MediaFormat
import android.media.MediaMuxer
import android.util.Log
import java.io.File

/**
 * Encodes a single utterance of PCM16 (mono, 16 kHz) into an Ogg/Opus file,
 * pumped incrementally: [start] once, [feed] PCM as it arrives, [finish] to
 * flush and read the bytes. Reused by both push-to-talk ([OpusRecorder]) and the
 * hands-free segmenter ([HandsFreeRecorder]).
 *
 * A fresh MediaCodec+MediaMuxer is needed per utterance (MediaMuxer can't be
 * restarted), so construct one of these per clip. All methods must be called
 * from the same (capture) thread.
 */
class OpusOggEncoder(
    private val sampleRate: Int = 16000,
    private val bitRate: Int = 24000,
) {
    private var codec: MediaCodec? = null
    private var muxer: MediaMuxer? = null
    private val info = MediaCodec.BufferInfo()
    private var trackIndex = -1
    private var muxing = false
    private var totalSamples = 0L
    private var outFile: File? = null

    fun start(file: File): Boolean {
        return try {
            val format = MediaFormat.createAudioFormat(MediaFormat.MIMETYPE_AUDIO_OPUS, sampleRate, 1).apply {
                setInteger(MediaFormat.KEY_BIT_RATE, bitRate)
            }
            val c = MediaCodec.createEncoderByType(MediaFormat.MIMETYPE_AUDIO_OPUS)
            c.configure(format, null, null, MediaCodec.CONFIGURE_FLAG_ENCODE)
            c.start()
            codec = c
            muxer = MediaMuxer(file.absolutePath, MediaMuxer.OutputFormat.MUXER_OUTPUT_OGG)
            outFile = file
            trackIndex = -1; muxing = false; totalSamples = 0L
            true
        } catch (e: Exception) {
            Log.e(TAG, "encoder start failed", e); release(); false
        }
    }

    /** Queue [len] bytes of PCM16 and drain any ready output to the muxer. */
    fun feed(pcm: ByteArray, len: Int) {
        val c = codec ?: return
        val inIdx = c.dequeueInputBuffer(10_000)
        if (inIdx >= 0) {
            val inBuf = c.getInputBuffer(inIdx)!!
            inBuf.clear(); inBuf.put(pcm, 0, len)
            c.queueInputBuffer(inIdx, 0, len, totalSamples * 1_000_000L / sampleRate, 0)
            totalSamples += len / 2
        }
        drain(false)
    }

    /** Signal end-of-stream, flush, and return the finished Ogg bytes (null on failure). */
    fun finish(): ByteArray? {
        val c = codec ?: return null
        return try {
            val inIdx = c.dequeueInputBuffer(50_000)
            if (inIdx >= 0) {
                c.queueInputBuffer(inIdx, 0, 0, totalSamples * 1_000_000L / sampleRate, MediaCodec.BUFFER_FLAG_END_OF_STREAM)
            }
            drain(true)
            outFile?.takeIf { it.length() > 0 }?.readBytes()
        } catch (e: Exception) {
            Log.e(TAG, "encoder finish failed", e); null
        } finally {
            release()
        }
    }

    private fun drain(endOfStream: Boolean) {
        val c = codec ?: return
        val m = muxer ?: return
        while (true) {
            val outIdx = c.dequeueOutputBuffer(info, 10_000)
            when {
                outIdx == MediaCodec.INFO_OUTPUT_FORMAT_CHANGED -> {
                    trackIndex = m.addTrack(c.outputFormat)
                    m.start(); muxing = true
                }
                outIdx < 0 -> if (!endOfStream) return
                else -> {
                    val buf = c.getOutputBuffer(outIdx)
                    if (buf != null && info.size > 0 && muxing &&
                        info.flags and MediaCodec.BUFFER_FLAG_CODEC_CONFIG == 0
                    ) {
                        m.writeSampleData(trackIndex, buf, info)
                    }
                    c.releaseOutputBuffer(outIdx, false)
                }
            }
            if (info.flags and MediaCodec.BUFFER_FLAG_END_OF_STREAM != 0) return
        }
    }

    private fun release() {
        runCatching { codec?.stop() }; runCatching { codec?.release() }
        runCatching { if (muxing) muxer?.stop() }; runCatching { muxer?.release() }
        codec = null; muxer = null; muxing = false; trackIndex = -1
    }

    companion object {
        private const val TAG = "SpawnerMic"
        const val SAMPLE_RATE = 16000
        const val BIT_RATE = 24000
    }
}

package com.bam.spawner.tts

import android.util.Log
import com.k2fsa.sherpa.onnx.OfflineTts
import com.k2fsa.sherpa.onnx.OfflineTtsConfig
import com.k2fsa.sherpa.onnx.OfflineTtsKokoroModelConfig
import com.k2fsa.sherpa.onnx.OfflineTtsModelConfig
import com.k2fsa.sherpa.onnx.OfflineTtsVitsModelConfig
import java.io.File
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicLong

/**
 * On-device speech synthesis through sherpa-onnx — Kokoro or Piper, whichever
 * model directory it is pointed at.
 *
 * The engine is deliberately a *source of PCM*, not a second playback path: it
 * pushes 16-bit mono samples into the same [Speaker] stream the server's audio
 * goes through, so routing (earpiece vs speaker), muting, barge-in and the
 * "speaking" indicator all behave identically no matter who synthesized the
 * audio. The only thing that differs is the sample rate, which the model
 * reports and [Speaker.streamBegin] now takes as an argument.
 *
 * sherpa emits samples incrementally through a callback, one sentence at a
 * time, so speech starts before the whole utterance is synthesized. Returning 0
 * from that callback aborts generation, which is how barge-in stops a
 * half-spoken reply without waiting for the model to finish.
 */
class LocalTts(private val speaker: Speaker) {
    // Synthesis is CPU-bound and must not overlap with itself: one worker means
    // utterances queue in order, matching the server's one-at-a-time contract.
    private val worker = Executors.newSingleThreadExecutor { r ->
        Thread(r, "tts-local").apply { isDaemon = true }
    }

    private var tts: OfflineTts? = null
    private var loadedDir: String? = null
    private var loadedEngine: String? = null

    /** Bumped by [stop]; a generation whose epoch is stale aborts at its next callback. */
    private val epoch = AtomicLong(0)

    /** True once a model is loaded and ready to synthesize. */
    @Volatile var ready: Boolean = false
        private set

    /** Non-empty when the last load failed — surfaced in settings. */
    @Volatile var error: String = ""
        private set

    /**
     * Point the engine at an unpacked model directory. Loading is slow (a
     * hundred MB of ONNX), so it happens on the worker thread and only when the
     * directory or engine actually changed — re-selecting the same voice is free.
     */
    fun load(engine: String, dir: File) {
        val path = dir.absolutePath
        worker.execute {
            if (loadedDir == path && loadedEngine == engine && tts != null) return@execute
            releaseNow()
            try {
                val config = buildConfig(engine, dir)
                    ?: throw IllegalStateException("model directory is missing required files")
                tts = OfflineTts(assetManager = null, config = config)
                loadedDir = path
                loadedEngine = engine
                error = ""
                ready = true
            } catch (e: Exception) {
                Log.w(TAG, "load $engine from $path failed", e)
                error = e.message ?: e::class.java.simpleName
                ready = false
            }
        }
    }

    /** Drop the loaded model (engine switched away, or the model was deleted). */
    fun unload() {
        stop()
        worker.execute { releaseNow() }
    }

    /**
     * Synthesize [text] and stream it to the speaker. [sid] is the speaker id for
     * multi-voice models (Kokoro); Piper models are single-voice and ignore it.
     *
     * Returns immediately — synthesis runs on the worker, and so does the model
     * load that may still be queued ahead of it. That's why failure is reported
     * through [onUnavailable] rather than a return value: whether the model
     * loaded isn't known yet at call time. It fires when the model didn't load or
     * produced no audio, and the caller uses it to fall back to another engine
     * instead of going silent.
     */
    fun speak(text: String, sid: Int, onUnavailable: () -> Unit = {}) {
        val mine = epoch.incrementAndGet()
        worker.execute {
            val engine = tts
            if (engine == null) { onUnavailable(); return@execute }
            // Superseded while we waited our turn in the queue (barge-in, or a
            // newer utterance) — don't start at all.
            if (epoch.get() != mine || speaker.isMuted()) return@execute
            val rate = engine.sampleRate()
            var begun = false
            // Loudest sample the model produced. Anything over 1.0 is hard-clipped
            // by the 16-bit conversion, which sounds like distortion on the loud
            // syllables — a different fault from underrun, so it's measured apart.
            var clipPeak = 0f
            // Deliberately an explicit Function1 object, not a lambda. sherpa's JNI
            // resolves the callback by looking up `invoke([F)Ljava/lang/Integer;` on
            // the object's class; Kotlin 2.x compiles lambdas to invokedynamic, and
            // the class D8 synthesizes for one has only the erased `invoke(Object)`,
            // so the native side aborts the process with a NoSuchMethodError. An
            // anonymous object is a real class and carries the specialized method.
            val onSamples = object : Function1<FloatArray, Int> {
                override fun invoke(samples: FloatArray): Int {
                    if (epoch.get() != mine || speaker.isMuted()) return 0
                    if (!begun) { speaker.streamBegin(rate, PREROLL_MS); begun = true }
                    var peak = 0f
                    for (s in samples) { val a = kotlin.math.abs(s); if (a > peak) peak = a }
                    if (peak > clipPeak) clipPeak = peak
                    speaker.streamWrite(toPcm16(samples))
                    return 1
                }
            }
            try {
                engine.generateWithCallback(
                    text = text, sid = sid, speed = 1.0f, callback = onSamples,
                )
            } catch (e: Exception) {
                Log.w(TAG, "synthesis failed", e)
            }
            if (begun) {
                if (clipPeak > 1f) Log.w(TAG, "output clipped: peak ${"%.2f".format(clipPeak)}")
                if (epoch.get() == mine) speaker.streamEnd() else speaker.streamStop()
            } else if (epoch.get() == mine && !speaker.isMuted()) {
                // Loaded but produced nothing (a broken model, or text espeak
                // couldn't phonemize) — don't swallow the utterance.
                onUnavailable()
            }
        }
    }

    /** Abort the in-flight utterance and drop anything queued behind it. */
    fun stop() {
        epoch.incrementAndGet()
    }

    private fun releaseNow() {
        runCatching { tts?.release() }
        tts = null
        loadedDir = null
        loadedEngine = null
        ready = false
    }

    private companion object {
        const val TAG = "LocalTts"

        /**
         * Build sherpa's config by *discovering* the files in [dir] rather than
         * hardcoding their names. The bundles name their ONNX file differently
         * per model and quantization (`model.onnx`, `model.int8.onnx`,
         * `en_US-kristin-medium.onnx`, …), and upstream renames them between
         * releases; scanning means a rename can't silently break a model.
         */
        fun buildConfig(engine: String, dir: File): OfflineTtsConfig? {
            val files = dir.listFiles()?.toList().orEmpty()
            val onnx = files.filter { it.isFile && it.name.endsWith(".onnx") }
                // Prefer the quantized weights when a bundle ships both.
                .sortedByDescending { it.name.contains("int8") }
                .firstOrNull() ?: return null
            val tokens = File(dir, "tokens.txt").takeIf { it.isFile } ?: return null
            val dataDir = File(dir, "espeak-ng-data").takeIf { it.isDirectory }?.absolutePath ?: ""
            val lexicon = File(dir, "lexicon.txt").takeIf { it.isFile }?.absolutePath ?: ""

            val model = when (engine) {
                TtsEngine.KOKORO -> {
                    val voices = File(dir, "voices.bin").takeIf { it.isFile } ?: return null
                    OfflineTtsModelConfig(
                        kokoro = OfflineTtsKokoroModelConfig(
                            model = onnx.absolutePath,
                            voices = voices.absolutePath,
                            tokens = tokens.absolutePath,
                            dataDir = dataDir,
                            lexicon = lexicon,
                        ),
                        numThreads = THREADS,
                        provider = "cpu",
                    )
                }
                TtsEngine.PIPER -> OfflineTtsModelConfig(
                    vits = OfflineTtsVitsModelConfig(
                        model = onnx.absolutePath,
                        tokens = tokens.absolutePath,
                        dataDir = dataDir,
                        lexicon = lexicon,
                    ),
                    numThreads = THREADS,
                    provider = "cpu",
                )
                else -> return null
            }
            // maxNumSentences = 1 keeps the callback firing per sentence, which is
            // what makes first audio arrive early instead of after the whole reply.
            return OfflineTtsConfig(model = model, maxNumSentences = 1)
        }

        // Phones this app targets are big.LITTLE with 4 performance cores; 2 is
        // the point where Kokoro stops scaling and starts fighting the recorder.
        const val THREADS = 2

        // Head start handed to the AudioTrack before playback begins. Kokoro
        // synthesizes at roughly real time on a phone, so without this the track
        // plays out as fast as the model fills it and every scheduling hiccup is
        // an underrun. 600 ms costs that much extra latency to first word and
        // buys a margin the model can actually fall behind within.
        const val PREROLL_MS = 600

        /** sherpa's float samples (-1..1) → the 16-bit little-endian mono PCM Speaker wants. */
        fun toPcm16(samples: FloatArray): ByteArray {
            val out = ByteArray(samples.size * 2)
            for (i in samples.indices) {
                val v = (samples[i] * 32767f).toInt().coerceIn(-32768, 32767)
                out[i * 2] = (v and 0xff).toByte()
                out[i * 2 + 1] = ((v shr 8) and 0xff).toByte()
            }
            return out
        }
    }
}

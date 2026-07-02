package com.bam.spawner.audio

/**
 * Energy-based voice-activity detection for the hands-free path. Fed the same
 * PCM16 frames continuously; fires [onStart] once speech is sustained for
 * [onsetMs] (rejects brief noise blips), then [onEnd] after [silenceMs] of
 * silence (end of utterance) or [maxMs] since onset (hard cap). Call [reset]
 * before the next utterance.
 */
class Endpointer(
    private val silenceMs: Int = 800,
    private val maxMs: Int = 15000,
    private var rmsThreshold: Double = 500.0,
    private val onsetMs: Int = 120,
    private val onStart: (() -> Unit)? = null,
    private val onEnd: () -> Unit,
) {
    private var speechStarted = false
    private var loudMs = 0    // sustained loud run before onset
    private var captureMs = 0 // elapsed since onset
    private var silentMs = 0
    private var fired = false

    fun reset() {
        speechStarted = false; loudMs = 0; captureMs = 0; silentMs = 0; fired = false
    }

    /** Raise the onset/continuation energy bar (e.g. during TTS playback). */
    fun setThreshold(v: Double) { rmsThreshold = v }

    /** frame is PCM16LE bytes for ~[frameMs] of audio. */
    fun feed(frame: ByteArray, frameMs: Int) {
        if (fired) return
        val loud = rms(frame) >= rmsThreshold
        if (!speechStarted) {
            // Idle: require a sustained run of speech before declaring onset.
            if (loud) {
                loudMs += frameMs
                if (loudMs >= onsetMs) {
                    speechStarted = true
                    onStart?.invoke()
                }
            } else {
                loudMs = 0
            }
            return
        }
        // Speaking: end on a run of silence, or a hard cap since onset.
        captureMs += frameMs
        if (loud) silentMs = 0 else silentMs += frameMs
        if (silentMs >= silenceMs || captureMs >= maxMs) {
            fired = true
            onEnd()
        }
    }

    private fun rms(frame: ByteArray): Double {
        if (frame.size < 2) return 0.0
        var sum = 0.0
        var i = 0
        val n = frame.size - 1
        while (i < n) {
            val sample = (frame[i].toInt() and 0xff) or (frame[i + 1].toInt() shl 8)
            sum += (sample * sample).toDouble()
            i += 2
        }
        return Math.sqrt(sum / (frame.size / 2))
    }
}

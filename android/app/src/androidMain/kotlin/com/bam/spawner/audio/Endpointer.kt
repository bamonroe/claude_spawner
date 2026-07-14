package com.bam.spawner.audio

/**
 * Energy-based voice-activity detection for the hands-free path. Fed the same
 * PCM16 frames continuously; fires [onStart] once speech is sustained for
 * [onsetMs] (rejects brief noise blips), then [onEnd] after [silenceMs] of
 * silence (end of utterance) or [maxMs] since onset (hard cap). Call [reset]
 * before the next utterance.
 *
 * When [adaptive] is on the energy bar isn't a fixed number: it rides above a
 * continuously-measured ambient noise floor (an EMA of the RMS of frames judged
 * non-speech) at [noiseRatio]×, never dropping below the configured
 * [rmsThreshold]. In a quiet room the floor is low so the configured threshold
 * governs (unchanged behaviour); in a noisy room the floor rises and the bar
 * lifts with it, rejecting steady background noise without the user re-tuning.
 */
class Endpointer(
    private val silenceMs: Int = 800,
    private val maxMs: Int = 15000,
    private var rmsThreshold: Double = 500.0,
    private val onsetMs: Int = 120,
    private val adaptive: Boolean = true,
    private val noiseRatio: Double = 2.5,
    private val onStart: (() -> Unit)? = null,
    private val onEnd: () -> Unit,
) {
    private var speechStarted = false
    private var loudMs = 0    // sustained loud run before onset
    private var captureMs = 0 // elapsed since onset
    private var silentMs = 0
    private var fired = false
    // Ambient noise floor (RMS), tracked only from non-speech frames so speech
    // never inflates it. Negative until the first frame seeds it; kept across
    // utterances (reset() leaves it intact) so calibration persists.
    private var noiseFloor = -1.0

    fun reset() {
        speechStarted = false; loudMs = 0; captureMs = 0; silentMs = 0; fired = false
    }

    /** Raise the onset/continuation energy bar (e.g. during TTS playback). */
    fun setThreshold(v: Double) { rmsThreshold = v }

    /** The bar a frame must clear now: the configured floor, or the ambient
     *  noise floor scaled up, whichever is higher. */
    private fun effectiveThreshold(): Double =
        if (!adaptive || noiseFloor < 0) rmsThreshold
        else maxOf(rmsThreshold, noiseFloor * noiseRatio)

    /** frame is PCM16LE bytes for ~[frameMs] of audio. */
    fun feed(frame: ByteArray, frameMs: Int) {
        if (fired) return
        val rms = pcm16Rms(frame, frame.size)
        val loud = rms >= effectiveThreshold()
        // Follow the ambient floor from quiet frames only (loud frames may be
        // speech). Rise slowly so a passing noise burst can't ratchet the bar up;
        // fall a little faster so the room going quiet re-lowers it promptly.
        if (adaptive && !loud) {
            noiseFloor =
                if (noiseFloor < 0) rms
                else {
                    val alpha = if (rms > noiseFloor) 0.02 else 0.05
                    noiseFloor + alpha * (rms - noiseFloor)
                }
        }
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

}

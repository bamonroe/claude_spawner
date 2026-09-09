package com.bam.spawner.tts

/**
 * The speech-synthesis engines the app can speak through, and the on-device
 * models they need. Shared (commonMain) so the settings UI renders the same
 * list everywhere; only Android can actually *run* the local engines, which is
 * why [AppController.localTtsSupported] gates them at the UI.
 *
 * Engine ids are the stored value of [Prefs.ttsEngine] — lowercase strings, per
 * this codebase's string-enum convention (see Prefs).
 */
object TtsEngine {
    /** Kokoro synthesized on the server and streamed down as PCM (the original path). */
    const val SERVER = "server"
    /** Android's own `android.speech.tts` engine — no download, always available. */
    const val ANDROID = "android"
    /** Kokoro running on this device via sherpa-onnx. Needs [KOKORO_MODEL]. */
    const val KOKORO = "kokoro"
    /** Piper (VITS) running on this device via sherpa-onnx. Needs a Piper voice model. */
    const val PIPER = "piper"

    val ALL = listOf(SERVER, ANDROID, KOKORO, PIPER)

    /** True when the engine synthesizes on this device and so needs a downloaded model. */
    fun isLocal(engine: String) = engine == KOKORO || engine == PIPER

    fun label(engine: String) = when (engine) {
        SERVER -> "Server Kokoro"
        ANDROID -> "Android"
        KOKORO -> "Local Kokoro"
        PIPER -> "Local Piper"
        else -> engine
    }
}

/**
 * One downloadable sherpa-onnx model bundle.
 *
 * [url] points at the k2-fsa `tts-models` release; the archive is a `.tar.bz2`
 * that unpacks to a single directory. We deliberately do **not** record the
 * names of the files inside it: the on-device loader discovers `*.onnx`,
 * `tokens.txt`, `voices.bin` and `espeak-ng-data/` by scanning the unpacked
 * directory, so upstream renaming a file can't silently break a model.
 *
 * [voices] is the speaker table for multi-speaker models (Kokoro), in speaker-id
 * order — sherpa selects a voice by integer id and its Kotlin API doesn't expose
 * the names, which live in the ONNX metadata. Single-voice models (Piper) leave
 * it empty: there, the *model* is the voice.
 */
data class TtsModel(
    val id: String,
    val engine: String,
    /** Human label for the settings list. */
    val label: String,
    val url: String,
    /** Download size in bytes, for the settings row and the progress bar. */
    val bytes: Long,
    val voices: List<String> = emptyList(),
    /** One-line note under the label (accent, quality tier). */
    val note: String = "",
)

/**
 * The models offered in Settings › Audio. Kept short on purpose: this is a
 * listen-and-compare bench for ruling engines in or out, not a voice store.
 *
 * Sizes are the real asset sizes from the k2-fsa `tts-models` release. Kokoro is
 * the int8 quantization (103 MB vs 320 MB for fp32) — the fp32 model is not
 * worth its size on a phone. Piper's int8 mediums are ~21 MB each, which is why
 * several fit here without ceremony.
 */
object TtsCatalogue {
    private const val BASE =
        "https://github.com/k2-fsa/sherpa-onnx/releases/download/tts-models/"

    /** Kokoro v0.19's 11 English speakers, in sherpa's speaker-id order. */
    val KOKORO_VOICES = listOf(
        "af", "af_bella", "af_nicole", "af_sarah", "af_sky",
        "am_adam", "am_michael",
        "bf_emma", "bf_isabella", "bm_george", "bm_lewis",
    )

    val KOKORO_MODEL = TtsModel(
        id = "kokoro-int8-en-v0_19",
        engine = TtsEngine.KOKORO,
        label = "Kokoro v0.19 (int8)",
        url = BASE + "kokoro-int8-en-v0_19.tar.bz2",
        bytes = 103_200_000L,
        voices = KOKORO_VOICES,
        note = "11 English voices, 24 kHz — the same model the server speaks with.",
    )

    val PIPER_MODELS = listOf(
        TtsModel(
            id = "vits-piper-en_US-kristin-medium-int8",
            engine = TtsEngine.PIPER,
            label = "Piper · en_US Kristin (medium)",
            url = BASE + "vits-piper-en_US-kristin-medium-int8.tar.bz2",
            bytes = 20_900_000L,
            note = "American female, medium quality.",
        ),
        TtsModel(
            id = "vits-piper-en_US-norman-medium-int8",
            engine = TtsEngine.PIPER,
            label = "Piper · en_US Norman (medium)",
            url = BASE + "vits-piper-en_US-norman-medium-int8.tar.bz2",
            bytes = 20_900_000L,
            note = "American male, medium quality.",
        ),
        TtsModel(
            id = "vits-piper-en_GB-jenny_dioco-medium-int8",
            engine = TtsEngine.PIPER,
            label = "Piper · en_GB Jenny (medium)",
            url = BASE + "vits-piper-en_GB-jenny_dioco-medium-int8.tar.bz2",
            bytes = 21_000_000L,
            note = "British female, medium quality.",
        ),
        TtsModel(
            id = "vits-piper-en_GB-alan-medium-int8",
            engine = TtsEngine.PIPER,
            label = "Piper · en_GB Alan (medium)",
            url = BASE + "vits-piper-en_GB-alan-medium-int8.tar.bz2",
            bytes = 21_100_000L,
            note = "British male, medium quality.",
        ),
    )

    val ALL: List<TtsModel> = listOf(KOKORO_MODEL) + PIPER_MODELS

    fun byId(id: String): TtsModel? = ALL.firstOrNull { it.id == id }

    fun forEngine(engine: String): List<TtsModel> = ALL.filter { it.engine == engine }
}

/** Install state of one [TtsModel] on this device, for the settings row. */
data class TtsModelState(
    val id: String,
    val installed: Boolean = false,
    /** Bytes fetched so far while a download is in flight; 0 otherwise. */
    val received: Long = 0,
    /** Total bytes of the in-flight download (0 = unknown). */
    val total: Long = 0,
    val downloading: Boolean = false,
    /** Non-empty when the last install attempt failed. */
    val error: String = "",
)

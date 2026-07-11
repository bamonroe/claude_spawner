package com.bam.spawner

/**
 * The typed, persisted settings the shared UI reads and writes. The Android
 * [SettingsStore] backs it with `SharedPreferences`; the web client backs it with
 * `localStorage`. Keeping it a plain interface (with the alias parsing as shared
 * default methods) means the settings screens can live in `commonMain` and both
 * clients persist the same keys with the same defaults — no divergence.
 *
 * The client-certificate material (the `.p12` file + passphrase) is deliberately
 * NOT here: that file I/O stays Android-only on the concrete [SettingsStore].
 */
interface Prefs {
    /** Server WebSocket URL. */
    var url: String
    /** Auth token presented in the hello handshake. */
    var token: String
    /** Stable per-install id so the server can resume our state on reconnect. */
    val clientId: String

    /** The session we were last attached to, to re-attach on reconnect. */
    var lastSession: String
    /** The last-attached session's stable id — preferred for re-attach (survives renames). */
    var lastSessionId: String

    /** Theme preference: "system" | "light" | "dark". */
    var themeMode: String
    /** Per-message token-usage badge detail: "off" | "compact" | "detailed". */
    var tokenBadge: String
    /** Show a status-bar indicator counting down the ~5-min warm prompt-cache window. */
    var cacheWarmTimer: Boolean
    /** Warm compress: compress a session past the limit just before its warm cache expires. */
    var warmCompress: Boolean
    /** Auto compress: compress a session as soon as it crosses the limit (no warm-window wait). */
    var autoCompress: Boolean
    /** Context-token limit (in thousands) that both compress triggers share. */
    var autoCompressThreshold: Int

    /** Whether hands-free (always-listening) mode is enabled. */
    var handsFree: Boolean
    /** Preferred TTS audio output: "earpiece" | "speaker" | "bluetooth". */
    var audioOutput: String
    /** Ask Claude for brief, TTS-friendly replies (appended as a prompt hint). */
    var brief: Boolean
    /** Let Claude ask clarifying questions mid-task instead of guessing. */
    var interactive: Boolean
    /** Spoken word that commits a hands-free message ("beep" by default). */
    var endToken: String
    /** Custom wake word, accepted alongside the built-in "hey buddy" (blank = built-in only). */
    var wakeToken: String

    /** Debug: draw translucent overlays over normally-invisible hit zones (e.g. the
     *  push-to-talk cancel / hands-free swipe thresholds) and log gesture end reasons. */
    var debugOverlays: Boolean

    /** Whisper model selection: "dynamic" (by clip length) or "fixed". */
    var sttMode: String
    /** Fixed-mode whisper model: "tiny" | "base" | "small". */
    var sttModel: String
    /** Resident whisper server URL (resolved on the server host); blank = server default. */
    var whisperUrl: String
    /** Resident whisper model to hot-load (ggml name, e.g. "medium.en"). */
    var whisperModel: String
    /** Fast (draft/detection, "quick" transcribe) whisper model; "" = server default/none. */
    var whisperFastModel: String

    /** Command aliases as "misheard = command" lines (fixes whisper mistakes). */
    var commandAliases: String

    /** Hands-free: auto-commit after this many seconds of silence (0 = never; end token only). */
    var silenceCommitSeconds: Float
    /** VAD energy bar: lower = more sensitive (catches quiet speech, more false triggers). */
    var vadThreshold: Int
    /** Sustained speech (ms) required before capture starts (rejects short noise blips). */
    var vadOnsetMs: Int
    /** Silence (ms) after speech that ends the utterance ("I'm done talking"). */
    var vadSilenceMs: Int

    /** Parse the alias lines into a misheard->canonical map. */
    fun aliasMap(): Map<String, String> = commandAliases.lines().mapNotNull { line ->
        val i = line.indexOf('=')
        if (i <= 0) return@mapNotNull null
        val k = line.substring(0, i).trim().lowercase()
        val v = line.substring(i + 1).trim().lowercase()
        if (k.isNotEmpty() && v.isNotEmpty()) k to v else null
    }.toMap()

    fun addAlias(from: String, to: String) {
        val f = from.trim().lowercase()
        val t = to.trim().lowercase()
        if (f.isEmpty() || t.isEmpty()) return
        commandAliases = (aliasMap() + (f to t)).entries.joinToString("\n") { "${it.key} = ${it.value}" }
    }

    fun removeAlias(from: String) {
        commandAliases = (aliasMap() - from.trim().lowercase()).entries.joinToString("\n") { "${it.key} = ${it.value}" }
    }

    companion object {
        // Server on the host, reached over Tailscale (works on wifi or cellular).
        // For the Android emulator instead, use ws://10.0.2.2:<port>/ws.
        const val DEFAULT_URL = "ws://100.64.0.2:8558/ws"
        const val DEFAULT_TOKEN = "devsecret"
        const val DEFAULT_ALIASES = "attached = attach\ndetached = detach\nthe tach = detach\nkilo = kill\nlisted = list"

        // Resolved on the SERVER host — the resident whisper container's published port.
        const val DEFAULT_WHISPER_URL = "http://localhost:8571"

        // Every non-zero default lives here, once, so the two Prefs backends
        // (SettingsStore / WebPrefs) can't drift apart. Defaults of false or ""
        // stay inline at the property — they're unambiguous zero values.
        const val DEFAULT_THEME_MODE = "system"
        const val DEFAULT_TOKEN_BADGE = "compact"
        const val DEFAULT_CACHE_WARM_TIMER = true
        const val DEFAULT_AUTO_COMPRESS_THRESHOLD_K = 100
        const val DEFAULT_AUDIO_OUTPUT = "earpiece"
        const val DEFAULT_END_TOKEN = "beep"
        const val DEFAULT_STT_MODE = "dynamic"
        const val DEFAULT_STT_MODEL = "small"
        const val DEFAULT_WHISPER_MODEL = "medium.en"
        const val DEFAULT_VAD_THRESHOLD = 500
        const val DEFAULT_VAD_ONSET_MS = 120
        const val DEFAULT_VAD_SILENCE_MS = 800
    }
}

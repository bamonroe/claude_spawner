package com.bam.spawner

import android.content.Context

/** Persists the server URL, auth token, and per-connection voice settings. */
class SettingsStore(context: Context) : Prefs {
    private val prefs = context.getSharedPreferences("spawner", Context.MODE_PRIVATE)

    override var url: String
        get() = prefs.getString("url", Prefs.DEFAULT_URL) ?: Prefs.DEFAULT_URL
        set(v) = prefs.edit().putString("url", v).apply()

    override var token: String
        get() = prefs.getString("token", Prefs.DEFAULT_TOKEN) ?: Prefs.DEFAULT_TOKEN
        set(v) = prefs.edit().putString("token", v).apply()

    /** Stable per-install id so the server can resume our state on reconnect. */
    override val clientId: String
        get() {
            prefs.getString("client_id", null)?.let { return it }
            val id = java.util.UUID.randomUUID().toString()
            prefs.edit().putString("client_id", id).apply()
            return id
        }

    /** The session we were last attached to, to re-attach on reconnect. */
    override var lastSession: String
        get() = prefs.getString("last_session", "") ?: ""
        set(v) = prefs.edit().putString("last_session", v).apply()

    /** The last-attached session's stable id — preferred for re-attach on reconnect
     *  (survives renames and identifies the same session across different servers). */
    override var lastSessionId: String
        get() = prefs.getString("last_session_id", "") ?: ""
        set(v) = prefs.edit().putString("last_session_id", v).apply()

    /** Theme preference: "system" | "light" | "dark". */
    override var themeMode: String
        get() = prefs.getString("theme_mode", "system") ?: "system"
        set(v) = prefs.edit().putString("theme_mode", v).apply()

    /** Per-message token-usage badge detail: "off" | "compact" | "detailed".
     *  Compact shows in/out totals; detailed adds the cache-read/write split. */
    override var tokenBadge: String
        get() = prefs.getString("token_badge", "compact") ?: "compact"
        set(v) = prefs.edit().putString("token_badge", v).apply()

    /** Show a status-bar indicator counting down the ~5-min warm prompt-cache window. */
    override var cacheWarmTimer: Boolean
        get() = prefs.getBoolean("cache_warm_timer", true)
        set(v) = prefs.edit().putBoolean("cache_warm_timer", v).apply()

    override var warmCompress: Boolean
        get() = prefs.getBoolean("warm_compress", false)
        set(v) = prefs.edit().putBoolean("warm_compress", v).apply()

    override var autoCompress: Boolean
        get() = prefs.getBoolean("auto_compress", false)
        set(v) = prefs.edit().putBoolean("auto_compress", v).apply()

    override var autoCompressThreshold: Int
        get() = prefs.getInt("auto_compress_threshold", 100)
        set(v) = prefs.edit().putInt("auto_compress_threshold", v).apply()

    /** Whether hands-free (always-listening) mode is enabled. */
    override var handsFree: Boolean
        get() = prefs.getBoolean("hands_free", false)
        set(v) = prefs.edit().putBoolean("hands_free", v).apply()

    /** Preferred TTS audio output: "earpiece" | "speaker" | "bluetooth". */
    override var audioOutput: String
        get() = prefs.getString("audio_output", "earpiece") ?: "earpiece"
        set(v) = prefs.edit().putString("audio_output", v).apply()

    /** Ask Claude for brief, TTS-friendly replies (appended as a prompt hint). */
    override var brief: Boolean
        get() = prefs.getBoolean("brief", false)
        set(v) = prefs.edit().putBoolean("brief", v).apply()

    /** Let Claude ask clarifying questions mid-task instead of guessing. */
    override var interactive: Boolean
        get() = prefs.getBoolean("interactive", false)
        set(v) = prefs.edit().putBoolean("interactive", v).apply()

    /** Spoken word that commits a hands-free message ("beep" by default). */
    override var endToken: String
        get() = prefs.getString("end_token", "beep")?.ifBlank { "beep" } ?: "beep"
        set(v) = prefs.edit().putString("end_token", v).apply()

    /** Custom wake word, accepted alongside built-in "hey buddy" (blank = built-in only). */
    override var wakeToken: String
        get() = prefs.getString("wake_token", "") ?: ""
        set(v) = prefs.edit().putString("wake_token", v).apply()

    /** Whisper model selection: "dynamic" (by clip length) or "fixed". */
    override var sttMode: String
        get() = prefs.getString("stt_mode", "dynamic") ?: "dynamic"
        set(v) = prefs.edit().putString("stt_mode", v).apply()

    /** Fixed-mode whisper model: "tiny" | "base" | "small". */
    override var sttModel: String
        get() = prefs.getString("stt_model", "small") ?: "small"
        set(v) = prefs.edit().putString("stt_model", v).apply()

    /** Resident whisper server URL (resolved on the server host); blank = server default. */
    override var whisperUrl: String
        get() = prefs.getString("whisper_url", Prefs.DEFAULT_WHISPER_URL) ?: Prefs.DEFAULT_WHISPER_URL
        set(v) = prefs.edit().putString("whisper_url", v).apply()

    /** Resident whisper model to hot-load (ggml name, e.g. "medium.en"). */
    override var whisperModel: String
        get() = prefs.getString("whisper_model", "medium.en") ?: "medium.en"
        set(v) = prefs.edit().putString("whisper_model", v).apply()

    /** Command aliases as "misheard = command" lines (fixes whisper mistakes). */
    override var commandAliases: String
        get() = prefs.getString("cmd_aliases", Prefs.DEFAULT_ALIASES) ?: Prefs.DEFAULT_ALIASES
        set(v) = prefs.edit().putString("cmd_aliases", v).apply()

    /** Hands-free: auto-commit after this many seconds of silence (0 = never; end token only). */
    override var silenceCommitSeconds: Float
        get() = prefs.getFloat("silence_commit_sec", 0f)
        set(v) = prefs.edit().putFloat("silence_commit_sec", v).apply()

    /** VAD energy bar: lower = more sensitive (catches quiet speech, more false triggers). */
    override var vadThreshold: Int
        get() = prefs.getInt("vad_threshold", 500)
        set(v) = prefs.edit().putInt("vad_threshold", v).apply()

    /** Sustained speech (ms) required before capture starts (rejects short noise blips). */
    override var vadOnsetMs: Int
        get() = prefs.getInt("vad_onset_ms", 120)
        set(v) = prefs.edit().putInt("vad_onset_ms", v).apply()

    /** Silence (ms) after speech that ends the utterance ("I'm done talking"). */
    override var vadSilenceMs: Int
        get() = prefs.getInt("vad_silence_ms", 800)
        set(v) = prefs.edit().putInt("vad_silence_ms", v).apply()

}

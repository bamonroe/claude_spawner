package com.bam.spawner

import kotlinx.browser.localStorage
import org.w3c.dom.get
import org.w3c.dom.set

private fun randomUuid(): String = js("crypto.randomUUID()")

/**
 * Browser-backed [Prefs]: every key persists in `localStorage`, mirroring the Android
 * `SettingsStore` so both clients keep the same settings with the same defaults. The
 * client-cert file I/O is Android-only and simply absent here (browser mTLS, if any, is
 * handled by the user's certificate store).
 */
class WebPrefs : Prefs {
    private fun str(k: String, d: String): String = localStorage[k] ?: d
    private fun putStr(k: String, v: String) { localStorage[k] = v }
    private fun bool(k: String, d: Boolean): Boolean = localStorage[k]?.toBooleanStrictOrNull() ?: d
    private fun putBool(k: String, v: Boolean) { localStorage[k] = v.toString() }
    private fun int(k: String, d: Int): Int = localStorage[k]?.toIntOrNull() ?: d
    private fun putInt(k: String, v: Int) { localStorage[k] = v.toString() }

    override var url: String
        get() = str("url", Prefs.DEFAULT_URL)
        set(v) = putStr("url", v)
    override var token: String
        get() = str("token", Prefs.DEFAULT_TOKEN)
        set(v) = putStr("token", v)
    override val clientId: String
        get() = localStorage["client_id"] ?: randomUuid().also { localStorage["client_id"] = it }

    override var lastSession: String
        get() = str("last_session", "")
        set(v) = putStr("last_session", v)
    override var lastSessionId: String
        get() = str("last_session_id", "")
        set(v) = putStr("last_session_id", v)

    override var themeMode: String
        get() = str("theme_mode", "system")
        set(v) = putStr("theme_mode", v)
    override var tokenBadge: String
        get() = str("token_badge", "compact")
        set(v) = putStr("token_badge", v)
    override var cacheWarmTimer: Boolean
        get() = bool("cache_warm_timer", true)
        set(v) = putBool("cache_warm_timer", v)

    override var handsFree: Boolean
        get() = bool("hands_free", false)
        set(v) = putBool("hands_free", v)
    override var audioOutput: String
        get() = str("audio_output", "earpiece")
        set(v) = putStr("audio_output", v)
    override var brief: Boolean
        get() = bool("brief", false)
        set(v) = putBool("brief", v)
    override var interactive: Boolean
        get() = bool("interactive", false)
        set(v) = putBool("interactive", v)
    override var endToken: String
        get() = str("end_token", "beep").ifBlank { "beep" }
        set(v) = putStr("end_token", v)

    override var sttMode: String
        get() = str("stt_mode", "dynamic")
        set(v) = putStr("stt_mode", v)
    override var sttModel: String
        get() = str("stt_model", "small")
        set(v) = putStr("stt_model", v)
    override var whisperUrl: String
        get() = str("whisper_url", Prefs.DEFAULT_WHISPER_URL)
        set(v) = putStr("whisper_url", v)
    override var whisperModel: String
        get() = str("whisper_model", "medium.en")
        set(v) = putStr("whisper_model", v)

    override var commandAliases: String
        get() = str("cmd_aliases", Prefs.DEFAULT_ALIASES)
        set(v) = putStr("cmd_aliases", v)

    override var silenceCommitSeconds: Float
        get() = localStorage["silence_commit_sec"]?.toFloatOrNull() ?: 0f
        set(v) { localStorage["silence_commit_sec"] = v.toString() }
    override var vadThreshold: Int
        get() = int("vad_threshold", 500)
        set(v) = putInt("vad_threshold", v)
    override var vadOnsetMs: Int
        get() = int("vad_onset_ms", 120)
        set(v) = putInt("vad_onset_ms", v)
    override var vadSilenceMs: Int
        get() = int("vad_silence_ms", 800)
        set(v) = putInt("vad_silence_ms", v)
}

package com.bam.spawner

import kotlinx.browser.localStorage
import org.w3c.dom.get
import org.w3c.dom.set

// `crypto.randomUUID()` exists only in a *secure context* (https or localhost). Served
// over plain http from a real host (e.g. a Tailscale name like claude.bam), the origin is
// insecure and `crypto.randomUUID` is undefined — calling it threw and broke the connect
// path, leaving the client stuck "disconnected". The client id is not security-sensitive
// (just a stable per-browser handle, persisted in localStorage), so fall back to a random
// string when the secure API is absent.
private fun randomUuid(): String = js(
    "(self.crypto && crypto.randomUUID) ? crypto.randomUUID() : " +
    "('c-' + Date.now().toString(36) + Math.random().toString(36).slice(2) + Math.random().toString(36).slice(2))"
)

// When the bundle is served by the spawner Go server itself, the gateway lives at
// "/ws" on the same origin — so default to that (wss when the page is https). The
// user can still override it under Settings → Server.
private fun sameOriginWsUrl(): String =
    js("(location.protocol === 'https:' ? 'wss://' : 'ws://') + location.host + '/ws'")

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
        get() = localStorage["url"] ?: sameOriginWsUrl()
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
        get() = str("theme_mode", Prefs.DEFAULT_THEME_MODE)
        set(v) = putStr("theme_mode", v)
    override var tokenBadge: String
        get() = str("token_badge", Prefs.DEFAULT_TOKEN_BADGE)
        set(v) = putStr("token_badge", v)
    override var cacheWarmTimer: Boolean
        get() = bool("cache_warm_timer", Prefs.DEFAULT_CACHE_WARM_TIMER)
        set(v) = putBool("cache_warm_timer", v)

    override var warmCompress: Boolean
        get() = bool("warm_compress", false)
        set(v) = putBool("warm_compress", v)

    override var autoCompress: Boolean
        get() = bool("auto_compress", false)
        set(v) = putBool("auto_compress", v)

    override var autoCompressThreshold: Int
        get() = int("auto_compress_threshold", Prefs.DEFAULT_AUTO_COMPRESS_THRESHOLD_K)
        set(v) = putInt("auto_compress_threshold", v)

    override var handsFree: Boolean
        get() = bool("hands_free", false)
        set(v) = putBool("hands_free", v)
    override var debugOverlays: Boolean
        get() = bool("debug_overlays", false)
        set(v) = putBool("debug_overlays", v)
    override var audioOutput: String
        get() = str("audio_output", Prefs.DEFAULT_AUDIO_OUTPUT)
        set(v) = putStr("audio_output", v)
    override var micSource: String
        get() = str("mic_source", Prefs.DEFAULT_MIC_SOURCE)
        set(v) = putStr("mic_source", v)
    override var brief: Boolean
        get() = bool("brief", false)
        set(v) = putBool("brief", v)
    override var interactive: Boolean
        get() = bool("interactive", false)
        set(v) = putBool("interactive", v)
    override var summaryOnlySpeech: Boolean
        get() = bool("summary_only_speech", false)
        set(v) = putBool("summary_only_speech", v)
    override var serverTts: Boolean
        get() = bool("server_tts", true)
        set(v) = putBool("server_tts", v)
    override var ttsVoice: String
        get() = str("tts_voice", "")
        set(v) = putStr("tts_voice", v)
    override var endToken: String
        get() = str("end_token", Prefs.DEFAULT_END_TOKEN).ifBlank { Prefs.DEFAULT_END_TOKEN }
        set(v) = putStr("end_token", v)
    override var wakeToken: String
        get() = str("wake_token", "")
        set(v) = putStr("wake_token", v)
    override var speakToken: String
        get() = str("speak_token", "")
        set(v) = putStr("speak_token", v)
    override var dictationGate: Boolean
        get() = bool("dictation_gate", false)
        set(v) = putBool("dictation_gate", v)

    override var sttMode: String
        get() = str("stt_mode", Prefs.DEFAULT_STT_MODE)
        set(v) = putStr("stt_mode", v)
    override var sttModel: String
        get() = str("stt_model", Prefs.DEFAULT_STT_MODEL)
        set(v) = putStr("stt_model", v)
    override var whisperUrl: String
        get() = str("whisper_url", Prefs.DEFAULT_WHISPER_URL)
        set(v) = putStr("whisper_url", v)
    override var whisperModel: String
        get() = str("whisper_model", Prefs.DEFAULT_WHISPER_MODEL)
        set(v) = putStr("whisper_model", v)
    override var whisperFastModel: String
        get() = str("whisper_fast_model", "")
        set(v) = putStr("whisper_fast_model", v)

    override var commandAliases: String
        get() = str("cmd_aliases", Prefs.DEFAULT_ALIASES)
        set(v) = putStr("cmd_aliases", v)

    override var trayCommands: String
        get() = str("tray_commands", Prefs.DEFAULT_TRAY_COMMANDS)
        set(v) = putStr("tray_commands", v)

    override var silenceCommitSeconds: Float
        get() = localStorage["silence_commit_sec"]?.toFloatOrNull() ?: 0f
        set(v) { localStorage["silence_commit_sec"] = v.toString() }
    override var vadThreshold: Int
        get() = int("vad_threshold", Prefs.DEFAULT_VAD_THRESHOLD)
        set(v) = putInt("vad_threshold", v)
    override var vadOnsetMs: Int
        get() = int("vad_onset_ms", Prefs.DEFAULT_VAD_ONSET_MS)
        set(v) = putInt("vad_onset_ms", v)
    override var vadSilenceMs: Int
        get() = int("vad_silence_ms", Prefs.DEFAULT_VAD_SILENCE_MS)
        set(v) = putInt("vad_silence_ms", v)
}

package com.bam.spawner

import android.content.Context

/** Persists the server URL, auth token, and (future) Picovoice key. */
class SettingsStore(context: Context) {
    private val prefs = context.getSharedPreferences("spawner", Context.MODE_PRIVATE)

    var url: String
        get() = prefs.getString("url", DEFAULT_URL) ?: DEFAULT_URL
        set(v) = prefs.edit().putString("url", v).apply()

    var token: String
        get() = prefs.getString("token", DEFAULT_TOKEN) ?: DEFAULT_TOKEN
        set(v) = prefs.edit().putString("token", v).apply()

    var picovoiceKey: String
        get() = prefs.getString("pvkey", "") ?: ""
        set(v) = prefs.edit().putString("pvkey", v).apply()

    /** Stable per-install id so the server can resume our state on reconnect. */
    val clientId: String
        get() {
            prefs.getString("client_id", null)?.let { return it }
            val id = java.util.UUID.randomUUID().toString()
            prefs.edit().putString("client_id", id).apply()
            return id
        }

    /** The session we were last attached to, to re-attach on reconnect. */
    var lastSession: String
        get() = prefs.getString("last_session", "") ?: ""
        set(v) = prefs.edit().putString("last_session", v).apply()

    /** Theme preference: "system" | "light" | "dark". */
    var themeMode: String
        get() = prefs.getString("theme_mode", "system") ?: "system"
        set(v) = prefs.edit().putString("theme_mode", v).apply()

    /** Whether hands-free (always-listening) mode is enabled. */
    var handsFree: Boolean
        get() = prefs.getBoolean("hands_free", false)
        set(v) = prefs.edit().putBoolean("hands_free", v).apply()

    /** Spoken word that commits a hands-free message ("beep" by default). */
    var endToken: String
        get() = prefs.getString("end_token", "beep")?.ifBlank { "beep" } ?: "beep"
        set(v) = prefs.edit().putString("end_token", v).apply()

    /** Whisper model selection: "dynamic" (by clip length) or "fixed". */
    var sttMode: String
        get() = prefs.getString("stt_mode", "dynamic") ?: "dynamic"
        set(v) = prefs.edit().putString("stt_mode", v).apply()

    /** Fixed-mode whisper model: "tiny" | "base" | "small". */
    var sttModel: String
        get() = prefs.getString("stt_model", "small") ?: "small"
        set(v) = prefs.edit().putString("stt_model", v).apply()

    /** Resident whisper server URL (resolved on the server host); blank = server default. */
    var whisperUrl: String
        get() = prefs.getString("whisper_url", DEFAULT_WHISPER_URL) ?: DEFAULT_WHISPER_URL
        set(v) = prefs.edit().putString("whisper_url", v).apply()

    /** Resident whisper model to hot-load (ggml name, e.g. "medium.en"). */
    var whisperModel: String
        get() = prefs.getString("whisper_model", "medium.en") ?: "medium.en"
        set(v) = prefs.edit().putString("whisper_model", v).apply()

    /** Command aliases as "misheard = command" lines (fixes whisper mistakes). */
    var commandAliases: String
        get() = prefs.getString("cmd_aliases", DEFAULT_ALIASES) ?: DEFAULT_ALIASES
        set(v) = prefs.edit().putString("cmd_aliases", v).apply()

    /** Parse the alias lines into a misheard->canonical map. */
    fun aliasMap(): Map<String, String> = commandAliases.lines().mapNotNull { line ->
        val i = line.indexOf('=')
        if (i <= 0) return@mapNotNull null
        val k = line.substring(0, i).trim().lowercase()
        val v = line.substring(i + 1).trim().lowercase()
        if (k.isNotEmpty() && v.isNotEmpty()) k to v else null
    }.toMap()

    private fun writeAliases(map: Map<String, String>) {
        commandAliases = map.entries.joinToString("\n") { "${it.key} = ${it.value}" }
    }

    fun addAlias(from: String, to: String) {
        val f = from.trim().lowercase()
        val t = to.trim().lowercase()
        if (f.isEmpty() || t.isEmpty()) return
        writeAliases(aliasMap() + (f to t))
    }

    fun removeAlias(from: String) = writeAliases(aliasMap() - from.trim().lowercase())

    /** Hands-free: auto-commit after this many seconds of silence (0 = never; end token only). */
    var silenceCommitSeconds: Float
        get() = prefs.getFloat("silence_commit_sec", 0f)
        set(v) = prefs.edit().putFloat("silence_commit_sec", v).apply()

    /** VAD energy bar: lower = more sensitive (catches quiet speech, more false triggers). */
    var vadThreshold: Int
        get() = prefs.getInt("vad_threshold", 500)
        set(v) = prefs.edit().putInt("vad_threshold", v).apply()

    /** Sustained speech (ms) required before capture starts (rejects short noise blips). */
    var vadOnsetMs: Int
        get() = prefs.getInt("vad_onset_ms", 120)
        set(v) = prefs.edit().putInt("vad_onset_ms", v).apply()

    /** Silence (ms) after speech that ends the utterance ("I'm done talking"). */
    var vadSilenceMs: Int
        get() = prefs.getInt("vad_silence_ms", 800)
        set(v) = prefs.edit().putInt("vad_silence_ms", v).apply()

    companion object {
        // Server on the host, reached over Tailscale (works on wifi or cellular).
        // For the Android emulator instead, use ws://10.0.2.2:<port>/ws.
        const val DEFAULT_URL = "ws://100.64.0.2:8555/ws"
        const val DEFAULT_TOKEN = "devsecret"
        const val DEFAULT_ALIASES = "attached = attach\ndetached = detach\nthe tach = detach\nkilo = kill\nlisted = list"

        // Resolved on the SERVER host — the resident whisper container's published port.
        const val DEFAULT_WHISPER_URL = "http://localhost:8571"
    }
}

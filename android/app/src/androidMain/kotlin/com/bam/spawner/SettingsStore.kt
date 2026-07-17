package com.bam.spawner

import android.content.Context
import java.io.File
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate

/** Persists the server URL, auth token, and per-connection voice settings. */
class SettingsStore(context: Context) : Prefs {
    private val prefs = context.getSharedPreferences("spawner", Context.MODE_PRIVATE)
    private val appContext = context.applicationContext

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
        get() = prefs.getString("theme_mode", Prefs.DEFAULT_THEME_MODE) ?: Prefs.DEFAULT_THEME_MODE
        set(v) = prefs.edit().putString("theme_mode", v).apply()

    /** Per-message token-usage badge detail: "off" | "compact" | "detailed".
     *  Compact shows in/out totals; detailed adds the cache-read/write split. */
    override var tokenBadge: String
        get() = prefs.getString("token_badge", Prefs.DEFAULT_TOKEN_BADGE) ?: Prefs.DEFAULT_TOKEN_BADGE
        set(v) = prefs.edit().putString("token_badge", v).apply()

    /** Show a status-bar indicator counting down the ~5-min warm prompt-cache window. */
    override var cacheWarmTimer: Boolean
        get() = prefs.getBoolean("cache_warm_timer", Prefs.DEFAULT_CACHE_WARM_TIMER)
        set(v) = prefs.edit().putBoolean("cache_warm_timer", v).apply()

    override var warmCompress: Boolean
        get() = prefs.getBoolean("warm_compress", false)
        set(v) = prefs.edit().putBoolean("warm_compress", v).apply()

    override var autoCompress: Boolean
        get() = prefs.getBoolean("auto_compress", false)
        set(v) = prefs.edit().putBoolean("auto_compress", v).apply()

    override var autoCompressThreshold: Int
        get() = prefs.getInt("auto_compress_threshold", Prefs.DEFAULT_AUTO_COMPRESS_THRESHOLD_K)
        set(v) = prefs.edit().putInt("auto_compress_threshold", v).apply()

    /** Whether hands-free (always-listening) mode is enabled. */
    override var handsFree: Boolean
        get() = prefs.getBoolean("hands_free", false)
        set(v) = prefs.edit().putBoolean("hands_free", v).apply()

    /** Debug overlays + verbose gesture logging (off by default). */
    override var debugOverlays: Boolean
        get() = prefs.getBoolean("debug_overlays", false)
        set(v) = prefs.edit().putBoolean("debug_overlays", v).apply()

    /** Preferred TTS audio output: "earpiece" | "speaker" | "bluetooth". */
    override var audioOutput: String
        get() = prefs.getString("audio_output", Prefs.DEFAULT_AUDIO_OUTPUT) ?: Prefs.DEFAULT_AUDIO_OUTPUT
        set(v) = prefs.edit().putString("audio_output", v).apply()

    /** Hands-free mic source: "phone" | "headset" (Bluetooth hands-free profile). */
    override var micSource: String
        get() = prefs.getString("mic_source", Prefs.DEFAULT_MIC_SOURCE) ?: Prefs.DEFAULT_MIC_SOURCE
        set(v) = prefs.edit().putString("mic_source", v).apply()

    /** Ask Claude for brief, TTS-friendly replies (appended as a prompt hint). */
    override var brief: Boolean
        get() = prefs.getBoolean("brief", false)
        set(v) = prefs.edit().putBoolean("brief", v).apply()

    /** Let Claude ask clarifying questions mid-task instead of guessing. */
    override var interactive: Boolean
        get() = prefs.getBoolean("interactive", false)
        set(v) = prefs.edit().putBoolean("interactive", v).apply()

    /** Speak only a turn's final result; intermediate steps beep (see Speaker.beep). */
    override var summaryOnlySpeech: Boolean
        get() = prefs.getBoolean("summary_only_speech", false)
        set(v) = prefs.edit().putBoolean("summary_only_speech", v).apply()

    /** In summary-only mode, speak the first N replies of each turn (rest beep). */
    override var speakInitialReplies: Int
        get() = prefs.getInt("speak_initial_replies", Prefs.DEFAULT_SPEAK_INITIAL_REPLIES)
        set(v) = prefs.edit().putInt("speak_initial_replies", v).apply()

    /** Server-side Kokoro voice vs on-device TTS (default on; falls back automatically). */
    override var serverTts: Boolean
        get() = prefs.getBoolean("server_tts", true)
        set(v) = prefs.edit().putBoolean("server_tts", v).apply()

    /** Chosen Kokoro voice ("" = the server default); rides each speak request. */
    override var ttsVoice: String
        get() = prefs.getString("tts_voice", "") ?: ""
        set(v) = prefs.edit().putString("tts_voice", v).apply()

    /** Spoken word that commits a hands-free message ("beep" by default). */
    override var endToken: String
        get() = prefs.getString("end_token", Prefs.DEFAULT_END_TOKEN)?.ifBlank { Prefs.DEFAULT_END_TOKEN } ?: Prefs.DEFAULT_END_TOKEN
        set(v) = prefs.edit().putString("end_token", v).apply()

    /** Custom wake word(s), comma-separated, alongside built-in "hey buddy" (blank = built-in only). */
    override var wakeToken: String
        get() = prefs.getString("wake_token", "") ?: ""
        set(v) = prefs.edit().putString("wake_token", v).apply()

    /** Dictation-gate start marker(s), comma-separated (blank = no gate token). */
    override var speakToken: String
        get() = prefs.getString("speak_token", "") ?: ""
        set(v) = prefs.edit().putString("speak_token", v).apply()

    /** Discard un-bracketed hands-free speech unless it follows the speak token. */
    override var dictationGate: Boolean
        get() = prefs.getBoolean("dictation_gate", false)
        set(v) = prefs.edit().putBoolean("dictation_gate", v).apply()

    /** Live wake/end-token scoring backend: "whisper" (default) or "detector". */
    override var wakeService: String
        get() = prefs.getString("wake_service", Prefs.DEFAULT_WAKE_SERVICE) ?: Prefs.DEFAULT_WAKE_SERVICE
        set(v) = prefs.edit().putString("wake_service", v).apply()

    /** Whisper model selection: "dynamic" (by clip length) or "fixed". */
    override var sttMode: String
        get() = prefs.getString("stt_mode", Prefs.DEFAULT_STT_MODE) ?: Prefs.DEFAULT_STT_MODE
        set(v) = prefs.edit().putString("stt_mode", v).apply()

    /** Fixed-mode whisper model: "tiny" | "base" | "small". */
    override var sttModel: String
        get() = prefs.getString("stt_model", Prefs.DEFAULT_STT_MODEL) ?: Prefs.DEFAULT_STT_MODEL
        set(v) = prefs.edit().putString("stt_model", v).apply()

    /** Resident whisper server URL (resolved on the server host); blank = server default. */
    override var whisperUrl: String
        get() = prefs.getString("whisper_url", Prefs.DEFAULT_WHISPER_URL) ?: Prefs.DEFAULT_WHISPER_URL
        set(v) = prefs.edit().putString("whisper_url", v).apply()

    /** Resident whisper model to hot-load (ggml name, e.g. "medium.en"). */
    override var whisperModel: String
        get() = prefs.getString("whisper_model", Prefs.DEFAULT_WHISPER_MODEL) ?: Prefs.DEFAULT_WHISPER_MODEL
        set(v) = prefs.edit().putString("whisper_model", v).apply()

    /** Fast (draft/detection, "quick" transcribe) whisper model; "" = server default/none. */
    override var whisperFastModel: String
        get() = prefs.getString("whisper_fast_model", "") ?: ""
        set(v) = prefs.edit().putString("whisper_fast_model", v).apply()

    /** Command aliases as "misheard = command" lines (fixes whisper mistakes). */
    override var commandAliases: String
        get() = prefs.getString("cmd_aliases", Prefs.DEFAULT_ALIASES) ?: Prefs.DEFAULT_ALIASES
        set(v) = prefs.edit().putString("cmd_aliases", v).apply()

    /** Command names shown in the swipe-up tray, comma-separated. */
    override var trayCommands: String
        get() = prefs.getString("tray_commands", Prefs.DEFAULT_TRAY_COMMANDS) ?: Prefs.DEFAULT_TRAY_COMMANDS
        set(v) = prefs.edit().putString("tray_commands", v).apply()

    /** Hands-free: auto-commit after this many seconds of silence (0 = never; end token only). */
    override var silenceCommitSeconds: Float
        get() = prefs.getFloat("silence_commit_sec", 0f)
        set(v) = prefs.edit().putFloat("silence_commit_sec", v).apply()

    /** VAD energy bar: lower = more sensitive (catches quiet speech, more false triggers). */
    override var vadThreshold: Int
        get() = prefs.getInt("vad_threshold", Prefs.DEFAULT_VAD_THRESHOLD)
        set(v) = prefs.edit().putInt("vad_threshold", v).apply()

    /** Sustained speech (ms) required before capture starts (rejects short noise blips). */
    override var vadOnsetMs: Int
        get() = prefs.getInt("vad_onset_ms", Prefs.DEFAULT_VAD_ONSET_MS)
        set(v) = prefs.edit().putInt("vad_onset_ms", v).apply()

    /** Silence (ms) after speech that ends the utterance ("I'm done talking"). */
    override var vadSilenceMs: Int
        get() = prefs.getInt("vad_silence_ms", Prefs.DEFAULT_VAD_SILENCE_MS)
        set(v) = prefs.edit().putInt("vad_silence_ms", v).apply()

    /** Adapt the VAD energy bar to the room's ambient noise floor (default on). */
    override var vadAdaptive: Boolean
        get() = prefs.getBoolean("vad_adaptive", Prefs.DEFAULT_VAD_ADAPTIVE)
        set(v) = prefs.edit().putBoolean("vad_adaptive", v).apply()

    /** Run the platform noise suppressor on the headset/media capture path too. */
    override var headsetNoiseSuppression: Boolean
        get() = prefs.getBoolean("headset_ns", Prefs.DEFAULT_HEADSET_NOISE_SUPPRESSION)
        set(v) = prefs.edit().putBoolean("headset_ns", v).apply()

    // --- Trusted CA (Android-only; not in the shared Prefs interface) --------
    // A PEM CA the app trusts on top of the system store, so it can reach a server
    // whose `wss://` cert is signed by a private CA (Caddy `tls internal`). Stored
    // as PEM text; the transport layer parses it (see HttpTransport.android.kt).

    /** The trusted CA in PEM form, or "" when none. */
    var caCertPem: String
        get() = prefs.getString("ca_cert_pem", "") ?: ""
        private set(v) = prefs.edit().putString("ca_cert_pem", v).apply()

    /** Human label for the trusted CA (its certificate subject CN), for the UI. */
    var caCertName: String
        get() = prefs.getString("ca_cert_name", "") ?: ""
        private set(v) = prefs.edit().putString("ca_cert_name", v).apply()

    fun hasCaCert(): Boolean = caCertPem.isNotBlank()

    /** Where `adb push` can drop a CA for hands-off import — the app's external
     *  files dir is writable over adb on a non-rooted device. Auto-imported on
     *  connect (see [autoImportPushedCa]). */
    val pushedCaFile: File
        get() = File(appContext.getExternalFilesDir(null), "caddy-root.crt")

    /** Parse and store [bytes] as the trusted CA. Returns the cert subject on success,
     *  or throws if the bytes aren't a valid X.509 certificate. */
    fun importCaCert(bytes: ByteArray): String {
        val cf = CertificateFactory.getInstance("X.509")
        val cert = bytes.inputStream().use { cf.generateCertificate(it) } as X509Certificate
        val pem = toPem(cert)
        val name = cert.subjectX500Principal.name
            .split(",").firstOrNull { it.trim().startsWith("CN=") }
            ?.trim()?.removePrefix("CN=") ?: cert.subjectX500Principal.name
        caCertPem = pem
        caCertName = name
        return name
    }

    /** If no CA is stored yet and one has been pushed to [pushedCaFile], import it.
     *  Lets me place the cert over adb and have the app pick it up with no tapping. */
    fun autoImportPushedCa() {
        if (hasCaCert()) return
        val f = pushedCaFile
        if (f.exists()) runCatching { importCaCert(f.readBytes()) }
    }

    fun clearCaCert() {
        caCertPem = ""
        caCertName = ""
    }

    private fun toPem(cert: X509Certificate): String {
        val b64 = android.util.Base64.encodeToString(cert.encoded, android.util.Base64.NO_WRAP)
        return "-----BEGIN CERTIFICATE-----\n" +
            b64.chunked(64).joinToString("\n") +
            "\n-----END CERTIFICATE-----\n"
    }
}

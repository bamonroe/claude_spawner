package com.bam.spawner.net

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.booleanOrNull
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.doubleOrNull
import kotlinx.serialization.json.intOrNull
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.longOrNull
import kotlinx.serialization.json.put
import kotlinx.serialization.json.putJsonObject

/**
 * Server -> app messages (see docs/protocol.md). Parsed from JSON with the
 * multiplatform kotlinx.serialization JsonElement API (was org.json; that is
 * JVM-only and this file is now shared between the Android and web clients).
 */
sealed interface ServerMsg {
    data class HelloOk(val serverVersion: String, val whisperModel: String, val whisperModelFast: String = "", val whisperModels: List<String> = emptyList(), val whisperModelsLocal: List<String> = emptyList()) : ServerMsg
    // The resident servers' current models, server-global: accurate ("full") +
    // fast draft/detection ("quick"; empty = no fast server configured). `models`
    // is the English-model catalogue offered as a picker; `local` is the subset
    // already downloaded — a catalogue entry not in `local` downloads on select.
    // Both empty = unknown → free text.
    data class WhisperModel(val model: String, val fastModel: String = "", val models: List<String> = emptyList(), val local: List<String> = emptyList()) : ServerMsg
    // Progress of an on-demand ggml model download the server runs when a client
    // picks a catalogue model that isn't on disk yet. total 0 = unknown size.
    data class WhisperDownload(val model: String, val fast: Boolean, val received: Long, val total: Long, val done: Boolean, val error: String) : ServerMsg
    data class Say(val text: String) : ServerMsg
    data class Transcript(val text: String, val final: Boolean) : ServerMsg
    data class Pending(val text: String) : ServerMsg // live hands-free draft buffer
    data class Calibration(val text: String) : ServerMsg // what the detection model heard for a sample
    data class Activity(val text: String) : ServerMsg // live "Claude is thinking / editing X" indicator
    data object Transcribing : ServerMsg // committed hands-free clip is being re-transcribed accurately
    data class Files(val files: List<String>) : ServerMsg // files changed this turn
    data class Dialog(val state: String, val prompt: String) : ServerMsg
    // usage/usageAt seed the context meter from the transcript's last turn on
    // attach (usageAt = that turn's unix seconds, for the cache-warm countdown).
    data class Attached(val name: String, val sessionId: String = "", val usage: TokenUsage? = null, val usageAt: Long = 0, val agent: String = "", val model: String = "") : ServerMsg
    data object Detached : ServerMsg
    data class ContextReset(val name: String) : ServerMsg // Claude context cleared → drop token accounting
    data class Renamed(val old: String, val name: String, val sessionId: String = "") : ServerMsg // attached session renamed → update title in place (matched by id)
    // usageAt (final message only) is the turn's completion unix seconds — anchors
    // the cache-warm countdown to the turn's real age even when delivered buffered.
    data class Output(val name: String, val text: String, val chunk: Boolean, val usage: TokenUsage? = null, val usageAt: Long = 0) : ServerMsg
    data class History(val name: String, val messages: List<HistMsg>, val more: Boolean, val count: Int = 0, val hash: String = "", val unchanged: Boolean = false) : ServerMsg
    data class ReadLast(val count: Int) : ServerMsg
    data class Discovered(val sessions: List<DiscoveredInfo>) : ServerMsg
    data class RateLimit(val info: RateLimitInfo) : ServerMsg // Claude plan's usage-window state
    data class Usage(val report: UsageReport) : ServerMsg // `/usage` report (session/weekly % used)
    data class UsageEstimate(val est: UsageEstimateInfo) : ServerMsg // drift-live server-global estimate
    data class Err(val code: String, val message: String) : ServerMsg
    data class TurnInterrupted(val name: String, val reason: String) : ServerMsg
    data class TurnStopped(val name: String) : ServerMsg // a turn was deliberately aborted
    data class Diff(val text: String) : ServerMsg // post-turn `git diff --stat` review
    data class Ask(val name: String, val questions: List<AskQuestion>) : ServerMsg // interactive clarification
    data object StopSpeaking : ServerMsg
    data class SpeechMode(val summaryOnly: Boolean) : ServerMsg // speak only the final result (intermediate steps beep) vs everything
    data class Listing(val path: String, val parent: String, val entries: List<BrowseEntry>) : ServerMsg
    data class FileSaved(val path: String) : ServerMsg // an upload landed on the target host
    data class FileData(val name: String, val path: String, val content: String) : ServerMsg // a download's base64 bytes
    data class Agents(val agents: List<AgentInfo>, val default: String) : ServerMsg // AI backend registry (for the new-session picker)
    data class HostList(val hosts: List<Host>) : ServerMsg // app-managed SSH host registry
    data class IdentityList(val identities: List<Identity>) : ServerMsg // app-managed SSH identities
    data class Digests(val items: List<SessionDigest>) : ServerMsg // per-session transcript digests for offline-cache validation
    data class Unknown(val type: String) : ServerMsg

    companion object {
        private val json = Json { ignoreUnknownKeys = true }

        fun parse(raw: String): ServerMsg {
            val o = json.parseToJsonElement(raw).jsonObject
            return when (o.str("type")) {
                "hello_ok" -> HelloOk(o.str("server_version"), o.str("whisper_model"), o.str("whisper_model_fast"), readStrings(o.arr("whisper_models")), readStrings(o.arr("whisper_models_local")))
                "whisper_model" -> WhisperModel(o.str("model"), o.str("fast_model"), readStrings(o.arr("whisper_models")), readStrings(o.arr("whisper_models_local")))
                "whisper_download" -> WhisperDownload(o.str("model"), o.bool("fast"), o.long("received"), o.long("total"), o.bool("done"), o.str("error"))
                "say" -> Say(o.str("text"))
                "transcript" -> Transcript(o.str("text"), o.bool("final", true))
                "pending" -> Pending(o.str("text"))
                "calibration" -> Calibration(o.str("text"))
                "activity" -> Activity(o.str("text"))
                "transcribing" -> Transcribing
                "files" -> Files(readStrings(o.arr("files")))
                "dialog" -> Dialog(o.str("state"), o.str("prompt"))
                "attached" -> Attached(o.str("name"), o.str("session_id"), readUsage(o.obj("usage")), o.long("usage_at"), o.str("agent"), o.str("model"))
                "detached" -> Detached
                "context_reset" -> ContextReset(o.str("name"))
                "renamed" -> Renamed(o.str("old"), o.str("name"), o.str("session_id"))
                "output" -> Output(o.str("name"), o.str("text"), o.bool("chunk", false), readUsage(o.obj("usage")), o.long("usage_at"))
                "history" -> History(o.str("name"), readHist(o.arr("messages")), o.bool("more"), o.int("count", 0), o.str("hash"), o.bool("unchanged", false))
                "read_last" -> ReadLast(o.int("count", 1))
                "discovered" -> Discovered(readDiscovered(o.arr("sessions")))
                "rate_limit" -> RateLimit(RateLimitInfo(
                    o.str("status"), o.long("resets_at"),
                    o.str("limit_type"), o.bool("using_overage"),
                ))
                "usage" -> Usage(UsageReport(
                    o.int("session_pct", -1), o.str("session_reset"),
                    o.int("week_pct", -1), o.str("week_reset"), o.str("text"),
                ))
                "usage_estimate" -> UsageEstimate(UsageEstimateInfo(
                    o.bool("calibrated"),
                    o.dbl("session_est_pct", -1.0), o.dbl("week_est_pct", -1.0),
                    o.dbl("session_real_pct", -1.0), o.dbl("week_real_pct", -1.0),
                    o.long("cum_tokens"), o.long("tokens_since_check"),
                    o.long("turns_since_check"), o.long("last_check_at"),
                    o.bool("bench_set"),
                    o.dbl("bench_sess_pct", -1.0), o.dbl("bench_week_pct", -1.0),
                    o.long("bench_tokens"), o.long("tokens_since_set"),
                ))
                "error" -> Err(o.str("code"), o.str("message"))
                "turn_interrupted" -> TurnInterrupted(o.str("name"), o.str("reason"))
                "turn_stopped" -> TurnStopped(o.str("name"))
                "diff" -> Diff(o.str("text"))
                "ask" -> Ask(o.str("name"), readAsk(o.arr("questions")))
                "stop_speaking" -> StopSpeaking
                "speech_mode" -> SpeechMode(o.bool("summary_only"))
                "listing" -> Listing(o.str("path"), o.str("parent"), readEntries(o.arr("entries")))
                "file_saved" -> FileSaved(o.str("path"))
                "file_data" -> FileData(o.str("name"), o.str("path"), o.str("content"))
                "agents" -> Agents(readAgents(o.arr("agents")), o.str("default"))
                "host_list" -> HostList(readHosts(o.arr("hosts")))
                "identity_list" -> IdentityList(readIdentities(o.arr("identities")))
                "digests" -> Digests(readDigests(o.arr("items")))
                else -> Unknown(o.str("type"))
            }
        }

        private fun readUsage(o: JsonObject?): TokenUsage? {
            if (o == null) return null
            return TokenUsage(o.int("input"), o.int("output"), o.int("cache_write"), o.int("cache_read"))
        }

        private fun readDiscovered(arr: JsonArray?): List<DiscoveredInfo> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { s ->
                DiscoveredInfo(
                    s.str("name"), s.str("dir"), s.str("session_id"),
                    s.long("last_active"), s.bool("active"), s.bool("registered"),
                    s.bool("busy"), s.str("target"), s.str("host"),
                    s.str("agent"), s.str("model"),
                )
            }
        }

        private fun readAsk(arr: JsonArray?): List<AskQuestion> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { q ->
                AskQuestion(q.str("q"), readStrings(q.arr("options")))
            }
        }

        private fun readDigests(arr: JsonArray?): List<SessionDigest> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { d ->
                SessionDigest(d.str("name"), d.str("session_id"), d.int("count", 0), d.str("hash"))
            }
        }

        private fun readHist(arr: JsonArray?): List<HistMsg> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { m ->
                HistMsg(m.int("index", -1), m.str("role"), m.str("text"), m.long("ts"), readUsage(m.obj("usage")))
            }
        }

        private fun readStrings(arr: JsonArray?): List<String> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonPrimitive.contentOrNull ?: "" }
        }

        private fun readEntries(arr: JsonArray?): List<BrowseEntry> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { e ->
                BrowseEntry(e.str("name"), e.str("path"), e.bool("repo"), e.bool("dir", true))
            }
        }

        private fun readAgents(arr: JsonArray?): List<AgentInfo> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { a ->
                AgentInfo(
                    a.str("id"), a.str("name"), a.str("default_model"),
                    (a.arr("models") ?: JsonArray(emptyList())).map { it.jsonObject.str("alias") },
                )
            }
        }

        private fun readHosts(arr: JsonArray?): List<Host> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { h ->
                Host(
                    h.str("name"), h.str("address"), h.str("user"),
                    h.int("port"), h.str("key_file"), h.str("identity"),
                    h.str("claude_bin"),
                )
            }
        }

        private fun readIdentities(arr: JsonArray?): List<Identity> {
            if (arr == null) return emptyList()
            return arr.map { it.jsonObject }.map { i ->
                Identity(i.str("name"), i.str("user"), i.str("public_key"), i.bool("has_password"))
            }
        }
    }
}

// --- JsonObject accessors mirroring org.json's opt* (missing/null → the default) ---
private fun JsonObject.str(key: String): String = this[key]?.jsonPrimitive?.contentOrNull ?: ""
private fun JsonObject.int(key: String, def: Int = 0): Int = this[key]?.jsonPrimitive?.intOrNull ?: def
private fun JsonObject.long(key: String, def: Long = 0L): Long = this[key]?.jsonPrimitive?.longOrNull ?: def
private fun JsonObject.bool(key: String, def: Boolean = false): Boolean = this[key]?.jsonPrimitive?.booleanOrNull ?: def
private fun JsonObject.dbl(key: String, def: Double = 0.0): Double = this[key]?.jsonPrimitive?.doubleOrNull ?: def
private fun JsonObject.obj(key: String): JsonObject? = this[key] as? JsonObject
private fun JsonObject.arr(key: String): JsonArray? = this[key] as? JsonArray

/**
 * Per-turn token accounting from the final `output` message (see docs/protocol.md).
 * cacheRead > 0 = a warm prompt-cache hit; cacheWrite > 0 = the cache was (re)built.
 */
data class TokenUsage(val input: Int, val output: Int, val cacheWrite: Int, val cacheRead: Int) {
    /** Total context tokens read this turn (fresh input + cached prefix). */
    val contextTokens: Int get() = input + cacheRead + cacheWrite
    /** True if this turn reused a warm cache rather than rebuilding it. */
    val warmHit: Boolean get() = cacheRead > 0
}

/**
 * One AI backend from the `agents` message: its id (sent in `spawn_at`), display
 * name, default model alias, and the model aliases it offers. Used by the
 * new-session picker to choose a backend + model.
 */
data class AgentInfo(val id: String, val name: String, val defaultModel: String, val models: List<String>)

/**
 * The Claude subscription's usage-window state (see docs/protocol.md `rate_limit`).
 * status is coarse ("allowed" until the cap nears — no exact remaining quota exists);
 * resetsAt is unix seconds; limitType names the binding window ("five_hour" | weekly).
 */
data class RateLimitInfo(val status: String, val resetsAt: Long, val limitType: String, val usingOverage: Boolean) {
    val allowed: Boolean get() = status.isEmpty() || status == "allowed"
}

/**
 * The Claude plan's usage report from `/usage` (see docs/protocol.md `usage`).
 * sessionPct / weekPct are percent-**used** (−1 when the server couldn't parse them);
 * `text` is the full report shown verbatim (headline + local contributing breakdown).
 */
data class UsageReport(
    val sessionPct: Int, val sessionReset: String,
    val weekPct: Int, val weekReset: String, val text: String,
)

/**
 * Server-global drift-live usage estimate (see docs/protocol.md `usage_estimate`),
 * aggregated across all sessions/clients. The `*EstPct` fields drift up each turn;
 * `*RealPct` are the last /usage calibration's true numbers. Percents are −1 (and
 * `calibrated` false) until the first /usage anchors the estimate.
 */
data class UsageEstimateInfo(
    val calibrated: Boolean,
    val sessionEstPct: Double, val weekEstPct: Double,
    val sessionRealPct: Double, val weekRealPct: Double,
    val cumTokens: Long, val tokensSinceCheck: Long,
    val turnsSinceCheck: Long, val lastCheckAt: Long,
    // Manual benchmark ("set" button): whether one is armed, the percentages/
    // odometer it was stamped at, and the tokens burned since (what "calc" divides).
    val benchSet: Boolean = false,
    val benchSessPct: Double = -1.0, val benchWeekPct: Double = -1.0,
    val benchTokens: Long = 0, val tokensSinceSet: Long = 0,
)

/** One past message from a session's server-served history. */
data class HistMsg(val index: Int, val role: String, val text: String, val ts: Long = 0L, val usage: TokenUsage? = null)

/** One session's transcript digest from the `digests` message: message `count`
 *  and an opaque content `hash` the app compares against its cached copy. */
data class SessionDigest(val name: String, val sessionId: String, val count: Int, val hash: String)

/** A Claude session found on disk (via `discover`); may be adopted into the app. */
data class DiscoveredInfo(
    val name: String,
    val dir: String,
    val sessionId: String,
    val lastActive: Long,   // unix seconds
    val active: Boolean,    // interactive claude open in a terminal at this dir
    val registered: Boolean, // already in the spawner registry
    val busy: Boolean = false, // a dictation turn is running for this session now
    val target: String = "",   // execution target ("sandbox") when not the default host
    val host: String = "",     // the SSH host this session runs on (for grouping)
    val agent: String = "",    // AI backend id ("codex"); empty = default (claude)
    val model: String = "",    // model alias stamped at spawn (opus/gpt-5.5/…)
)

/** A directory in the "new session" browser. */
data class BrowseEntry(val name: String, val path: String, val repo: Boolean, val dir: Boolean = true)

/**
 * A configured SSH host for SSH-native execution (Settings → Hosts). The app is the
 * source of truth; the server persists the list. `address` is dialed literally (not
 * an ~/.ssh/config alias). Empty optional fields fall back to server defaults.
 */
data class Host(
    val name: String,
    val address: String,
    val user: String = "",
    val port: Int = 0,
    val keyFile: String = "",
    val identity: String = "", // name of a managed Identity; supersedes keyFile when set
    val claudeBin: String = "",
)

/** A managed SSH identity: a login credential the server holds. Carries a required
 *  default `user`, an optional keypair (public key shown; private key stays server-side),
 *  and optionally an SSH password (never sent — only `hasPassword` is reported). */
data class Identity(
    val name: String,
    val user: String = "",
    val publicKey: String = "",
    val hasPassword: Boolean = false,
)

/** One clarification Claude asked (interactive mode). Empty options = free-text. */
data class AskQuestion(val q: String, val options: List<String>)

/** Per-connection preferences sent in the hello handshake. */
data class HelloConfig(
    val endToken: String,
    val wakeToken: String,
    val speakToken: String,
    val dictationGate: Boolean,
    val sttMode: String,
    val sttModel: String,
    val aliases: Map<String, String>,
    val whisperUrl: String,
    val brief: Boolean,
    val interactive: Boolean,
    val warmCompress: Boolean,
    val autoCompress: Boolean,
    val autoCompressThreshold: Int,
)

/**
 * Audio codecs a `wake` message may declare. Mirrors the Go constants in
 * server/internal/gateway/audio.go; the server rejects anything else with a
 * `bad_message` error, and a docsync test keeps the two sets identical.
 */
object Codecs {
    /** Ogg/Opus clip (MediaRecorder) — what the Android app records. */
    const val OGG_OPUS = "ogg_opus"
    /** Raw PCM16LE / 16 kHz / mono — what the web client's Web Audio path sends. */
    const val PCM16 = "pcm16"
}

/** app -> server message builders. */
object Outbound {
    fun hello(token: String, clientId: String, cfg: HelloConfig) = buildJsonObject {
        put("type", "hello"); put("token", token); put("client_id", clientId)
        put("end_token", cfg.endToken); put("wake_token", cfg.wakeToken)
        put("speak_token", cfg.speakToken); put("dictation_gate", cfg.dictationGate)
        put("stt_mode", cfg.sttMode); put("stt_model", cfg.sttModel)
        putJsonObject("aliases") { for ((k, v) in cfg.aliases) put(k, v) }
        put("whisper_url", cfg.whisperUrl); put("brief", cfg.brief); put("interactive", cfg.interactive)
        put("warm_compress", cfg.warmCompress); put("auto_compress", cfg.autoCompress)
        put("auto_compress_threshold", cfg.autoCompressThreshold)
    }.toString()
    // Live-update the server-global context-compression preference without reconnecting.
    fun autoCompress(warm: Boolean, auto: Boolean, thresholdK: Int) = buildJsonObject {
        put("type", "auto_compress"); put("warm_compress", warm); put("auto_compress", auto)
        put("auto_compress_threshold", thresholdK)
    }.toString()
    fun utterance(text: String) = buildJsonObject { put("type", "utterance"); put("text", text) }.toString()
    fun usage() = buildJsonObject { put("type", "usage") }.toString() // fetch the plan's /usage report
    fun usageSet() = buildJsonObject { put("type", "usage_set") }.toString() // arm the two-point rate benchmark
    fun usageCalc() = buildJsonObject { put("type", "usage_calc") }.toString() // derive the rate from the benchmark
    fun abort() = buildJsonObject { put("type", "abort") }.toString() // cancel the running turn
    // fast targets the draft/detection ("quick" transcribe) server; default is the
    // accurate ("full" transcribe) one.
    fun setWhisperModel(model: String, fast: Boolean = false) =
        buildJsonObject {
            put("type", "set_whisper_model"); put("whisper_model", model)
            if (fast) put("fast", true)
        }.toString()
    fun restart() = buildJsonObject { put("type", "restart") }.toString() // ask the server to restart

    fun wake(codec: String, handsFree: Boolean = false, calibrate: Boolean = false) =
        buildJsonObject {
            put("type", "wake"); put("codec", codec)
            put("hands_free", handsFree); put("calibrate", calibrate)
        }.toString()
    fun audioEnd() = buildJsonObject { put("type", "audio_end") }.toString()
    fun commit() = buildJsonObject { put("type", "commit") }.toString() // silence-timeout commit
    fun discardDraft() = buildJsonObject { put("type", "discard_draft") }.toString() // drop uncommitted hands-free draft
    fun attach(name: String, sessionId: String = "", silent: Boolean = false) = buildJsonObject {
        put("type", "attach"); put("name", name)
        if (sessionId.isNotEmpty()) put("session_id", sessionId)
        put("silent", silent)
    }.toString()
    fun detach() = buildJsonObject { put("type", "detach") }.toString()
    fun history(name: String, before: Int?, limit: Int = 30, haveHash: String = "") = buildJsonObject {
        put("type", "history"); put("name", name); put("limit", limit)
        if (before != null) put("before", before)
        if (haveHash.isNotEmpty()) put("have_hash", haveHash) // top-page freshness check → server may reply `unchanged`
    }.toString()
    fun digest() = buildJsonObject { put("type", "digest") }.toString() // request all sessions' transcript digests (connect-time cache validation)
    fun discover() = buildJsonObject { put("type", "discover") }.toString()
    fun adopt(sessionId: String, dir: String) =
        buildJsonObject { put("type", "adopt"); put("session_id", sessionId); put("path", dir) }.toString()
    fun deleteDiscovered(sessionId: String) =
        buildJsonObject { put("type", "delete_discovered"); put("session_id", sessionId) }.toString()
    fun renameDiscovered(sessionId: String, dir: String, newName: String) = buildJsonObject {
        put("type", "rename_discovered"); put("session_id", sessionId)
        put("path", dir); put("new_name", newName)
    }.toString()
    // Switch a session's AI backend + model durably (sidebar Edit dialog). A blank
    // agent/model means the default backend / that backend's default model; changing
    // the backend restarts the conversation on the new AI (see docs/protocol.md).
    fun setAgent(sessionId: String, dir: String, agent: String, model: String) = buildJsonObject {
        put("type", "set_agent"); put("session_id", sessionId); put("path", dir)
        if (agent.isNotEmpty()) put("agent", agent)
        if (model.isNotEmpty()) put("model", model)
    }.toString()
    fun browse(path: String, host: String = "", files: Boolean = false) = buildJsonObject {
        put("type", "browse"); put("path", path)
        if (host.isNotEmpty()) put("host_name", host)
        if (files) put("files", true)
    }.toString()
    // File transfer over the socket: upload writes base64 bytes to <path>/<name> on
    // host; download reads the file at path on host (-> file_data). "" host = local.
    fun upload(path: String, name: String, contentB64: String, host: String = "") = buildJsonObject {
        put("type", "upload"); put("path", path); put("name", name); put("content", contentB64)
        if (host.isNotEmpty()) put("host_name", host)
    }.toString()
    fun download(path: String, host: String = "") = buildJsonObject {
        put("type", "download"); put("path", path)
        if (host.isNotEmpty()) put("host_name", host)
    }.toString()
    fun spawnAt(path: String, create: Boolean = false, target: String = "", host: String = "", agent: String = "", model: String = "") = buildJsonObject {
        put("type", "spawn_at"); put("path", path); put("create", create)
        if (target.isNotEmpty()) put("target", target)
        if (host.isNotEmpty()) put("host_name", host)
        if (agent.isNotEmpty()) put("agent", agent)
        if (model.isNotEmpty()) put("model", model)
    }.toString()

    // SSH host registry (Settings → Hosts). The server persists these and broadcasts
    // an updated host_list after every put/delete.
    fun hostsList() = buildJsonObject { put("type", "hosts") }.toString()
    fun hostPut(h: Host) = buildJsonObject {
        put("type", "host_put")
        putJsonObject("host") {
            put("name", h.name); put("address", h.address)
            put("user", h.user); put("port", h.port)
            put("key_file", h.keyFile); put("identity", h.identity); put("claude_bin", h.claudeBin)
        }
    }.toString()
    fun hostDelete(name: String) = buildJsonObject { put("type", "host_delete"); put("name", name) }.toString()

    // SSH identities (Settings → Identities). The server holds the private keys and
    // broadcasts an updated identity_list after every create/delete.
    fun identitiesList() = buildJsonObject { put("type", "identities") }.toString()
    fun identityCreate(name: String, user: String, password: String, genKey: Boolean) = buildJsonObject {
        put("type", "identity_create"); put("name", name); put("user", user)
        put("password", password); put("gen_key", genKey)
    }.toString()
    fun identityImport(name: String, user: String, password: String, keyPath: String) = buildJsonObject {
        put("type", "identity_import"); put("name", name); put("user", user)
        put("password", password); put("key_path", keyPath)
    }.toString()
    fun identityUpdate(name: String, user: String, setPassword: Boolean, password: String) = buildJsonObject {
        put("type", "identity_update"); put("name", name); put("user", user)
        put("set_password", setPassword); put("password", password)
    }.toString()
    fun identityDelete(name: String) = buildJsonObject { put("type", "identity_delete"); put("name", name) }.toString()
}

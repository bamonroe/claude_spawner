package com.bam.spawner.net

import org.json.JSONArray
import org.json.JSONObject

/**
 * Server -> app messages (see docs/protocol.md). Parsed from JSON with the
 * built-in org.json to avoid a serialization dependency.
 */
sealed interface ServerMsg {
    data class HelloOk(val serverVersion: String, val whisperModel: String) : ServerMsg
    data class WhisperModel(val model: String) : ServerMsg // resident server's current model (server-global)
    data class Say(val text: String) : ServerMsg
    data class Transcript(val text: String, val final: Boolean) : ServerMsg
    data class Pending(val text: String) : ServerMsg // live hands-free draft buffer
    data class Calibration(val text: String) : ServerMsg // what the detection model heard for a sample
    data class Activity(val text: String) : ServerMsg // live "Claude is thinking / editing X" indicator
    data class Files(val files: List<String>) : ServerMsg // files changed this turn
    data class Dialog(val state: String, val prompt: String) : ServerMsg
    // usage/usageAt seed the context meter from the transcript's last turn on
    // attach (usageAt = that turn's unix seconds, for the cache-warm countdown).
    data class Attached(val name: String, val sessionId: String = "", val usage: TokenUsage? = null, val usageAt: Long = 0) : ServerMsg
    data object Detached : ServerMsg
    data class ContextReset(val name: String) : ServerMsg // Claude context cleared → drop token accounting
    data class Renamed(val old: String, val name: String, val sessionId: String = "") : ServerMsg // attached session renamed → update title in place (matched by id)
    data class Output(val name: String, val text: String, val chunk: Boolean, val usage: TokenUsage? = null) : ServerMsg
    data class History(val name: String, val messages: List<HistMsg>, val more: Boolean) : ServerMsg
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
    data class Listing(val path: String, val parent: String, val entries: List<BrowseEntry>) : ServerMsg
    data class HostList(val hosts: List<Host>) : ServerMsg // app-managed SSH host registry
    data class Unknown(val type: String) : ServerMsg

    companion object {
        fun parse(raw: String): ServerMsg {
            val o = JSONObject(raw)
            return when (o.optString("type")) {
                "hello_ok" -> HelloOk(o.optString("server_version"), o.optString("whisper_model"))
                "whisper_model" -> WhisperModel(o.optString("model"))
                "say" -> Say(o.optString("text"))
                "transcript" -> Transcript(o.optString("text"), o.optBoolean("final", true))
                "pending" -> Pending(o.optString("text"))
                "calibration" -> Calibration(o.optString("text"))
                "activity" -> Activity(o.optString("text"))
                "files" -> Files(readStrings(o.optJSONArray("files")))
                "dialog" -> Dialog(o.optString("state"), o.optString("prompt"))
                "attached" -> Attached(o.optString("name"), o.optString("session_id"), readUsage(o.optJSONObject("usage")), o.optLong("usage_at"))
                "detached" -> Detached
                "context_reset" -> ContextReset(o.optString("name"))
                "renamed" -> Renamed(o.optString("old"), o.optString("name"), o.optString("session_id"))
                "output" -> Output(o.optString("name"), o.optString("text"), o.optBoolean("chunk", false), readUsage(o.optJSONObject("usage")))
                "history" -> History(o.optString("name"), readHist(o.optJSONArray("messages")), o.optBoolean("more"))
                "read_last" -> ReadLast(o.optInt("count", 1))
                "discovered" -> Discovered(readDiscovered(o.optJSONArray("sessions")))
                "rate_limit" -> RateLimit(RateLimitInfo(
                    o.optString("status"), o.optLong("resets_at"),
                    o.optString("limit_type"), o.optBoolean("using_overage"),
                ))
                "usage" -> Usage(UsageReport(
                    o.optInt("session_pct", -1), o.optString("session_reset"),
                    o.optInt("week_pct", -1), o.optString("week_reset"), o.optString("text"),
                ))
                "usage_estimate" -> UsageEstimate(UsageEstimateInfo(
                    o.optBoolean("calibrated"),
                    o.optDouble("session_est_pct", -1.0), o.optDouble("week_est_pct", -1.0),
                    o.optDouble("session_real_pct", -1.0), o.optDouble("week_real_pct", -1.0),
                    o.optLong("cum_tokens"), o.optLong("tokens_since_check"),
                    o.optLong("turns_since_check"), o.optLong("last_check_at"),
                    o.optBoolean("bench_set"),
                    o.optDouble("bench_sess_pct", -1.0), o.optDouble("bench_week_pct", -1.0),
                    o.optLong("bench_tokens"), o.optLong("tokens_since_set"),
                ))
                "error" -> Err(o.optString("code"), o.optString("message"))
                "turn_interrupted" -> TurnInterrupted(o.optString("name"), o.optString("reason"))
                "turn_stopped" -> TurnStopped(o.optString("name"))
                "diff" -> Diff(o.optString("text"))
                "ask" -> Ask(o.optString("name"), readAsk(o.optJSONArray("questions")))
                "stop_speaking" -> StopSpeaking
                "listing" -> Listing(o.optString("path"), o.optString("parent"), readEntries(o.optJSONArray("entries")))
                "host_list" -> HostList(readHosts(o.optJSONArray("hosts")))
                else -> Unknown(o.optString("type"))
            }
        }

        private fun readUsage(o: JSONObject?): TokenUsage? {
            if (o == null) return null
            return TokenUsage(o.optInt("input"), o.optInt("output"), o.optInt("cache_write"), o.optInt("cache_read"))
        }

        private fun readDiscovered(arr: JSONArray?): List<DiscoveredInfo> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val s = arr.getJSONObject(it)
                DiscoveredInfo(
                    s.optString("name"), s.optString("dir"), s.optString("session_id"),
                    s.optLong("last_active"), s.optBoolean("active"), s.optBoolean("registered"),
                    s.optBoolean("busy"), s.optString("target"),
                )
            }
        }

        private fun readAsk(arr: JSONArray?): List<AskQuestion> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val q = arr.getJSONObject(it)
                val opts = q.optJSONArray("options")
                AskQuestion(
                    q.optString("q"),
                    if (opts == null) emptyList() else (0 until opts.length()).map { i -> opts.optString(i) },
                )
            }
        }

        private fun readHist(arr: JSONArray?): List<HistMsg> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val m = arr.getJSONObject(it)
                HistMsg(m.optInt("index", -1), m.optString("role"), m.optString("text"), m.optLong("ts"), readUsage(m.optJSONObject("usage")))
            }
        }

        private fun readStrings(arr: JSONArray?): List<String> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map { arr.optString(it) }
        }

        private fun readEntries(arr: JSONArray?): List<BrowseEntry> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val e = arr.getJSONObject(it)
                BrowseEntry(e.optString("name"), e.optString("path"), e.optBoolean("repo"))
            }
        }

        private fun readHosts(arr: JSONArray?): List<Host> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val h = arr.getJSONObject(it)
                Host(
                    h.optString("name"), h.optString("address"), h.optString("user"),
                    h.optInt("port"), h.optString("key_file"), h.optString("claude_bin"),
                )
            }
        }
    }
}

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
)

/** A directory in the "new session" browser. */
data class BrowseEntry(val name: String, val path: String, val repo: Boolean)

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
    val claudeBin: String = "",
)

/** One clarification Claude asked (interactive mode). Empty options = free-text. */
data class AskQuestion(val q: String, val options: List<String>)

/** Per-connection preferences sent in the hello handshake. */
data class HelloConfig(
    val endToken: String,
    val sttMode: String,
    val sttModel: String,
    val aliases: Map<String, String>,
    val whisperUrl: String,
    val brief: Boolean,
    val interactive: Boolean,
)

/** app -> server message builders. */
object Outbound {
    fun hello(token: String, clientId: String, cfg: HelloConfig) =
        JSONObject().put("type", "hello").put("token", token).put("client_id", clientId)
            .put("end_token", cfg.endToken).put("stt_mode", cfg.sttMode).put("stt_model", cfg.sttModel)
            .put("aliases", JSONObject(cfg.aliases)).put("whisper_url", cfg.whisperUrl)
            .put("brief", cfg.brief).put("interactive", cfg.interactive)
            .toString()
    fun utterance(text: String) = JSONObject().put("type", "utterance").put("text", text).toString()
    fun usage() = JSONObject().put("type", "usage").toString() // fetch the plan's /usage report
    fun usageSet() = JSONObject().put("type", "usage_set").toString() // arm the two-point rate benchmark
    fun usageCalc() = JSONObject().put("type", "usage_calc").toString() // derive the rate from the benchmark
    fun abort() = JSONObject().put("type", "abort").toString() // cancel the running turn
    fun setWhisperModel(model: String) =
        JSONObject().put("type", "set_whisper_model").put("whisper_model", model).toString()
    fun restart() = JSONObject().put("type", "restart").toString() // ask the server to restart

    fun wake(codec: String, handsFree: Boolean = false, calibrate: Boolean = false) =
        JSONObject().put("type", "wake").put("codec", codec)
            .put("hands_free", handsFree).put("calibrate", calibrate).toString()
    fun audioEnd() = JSONObject().put("type", "audio_end").toString()
    fun commit() = JSONObject().put("type", "commit").toString() // silence-timeout commit
    fun discardDraft() = JSONObject().put("type", "discard_draft").toString() // drop uncommitted hands-free draft
    fun attach(name: String, sessionId: String = "", silent: Boolean = false) =
        JSONObject().put("type", "attach").put("name", name)
            .apply { if (sessionId.isNotEmpty()) put("session_id", sessionId) }
            .put("silent", silent).toString()
    fun detach() = JSONObject().put("type", "detach").toString()
    fun history(name: String, before: Int?, limit: Int = 30): String {
        val o = JSONObject().put("type", "history").put("name", name).put("limit", limit)
        if (before != null) o.put("before", before)
        return o.toString()
    }
    fun discover() = JSONObject().put("type", "discover").toString()
    fun adopt(sessionId: String, dir: String) =
        JSONObject().put("type", "adopt").put("session_id", sessionId).put("path", dir).toString()
    fun deleteDiscovered(sessionId: String) =
        JSONObject().put("type", "delete_discovered").put("session_id", sessionId).toString()
    fun renameDiscovered(sessionId: String, dir: String, newName: String) =
        JSONObject().put("type", "rename_discovered").put("session_id", sessionId)
            .put("path", dir).put("new_name", newName).toString()
    fun browse(path: String) = JSONObject().put("type", "browse").put("path", path).toString()
    fun spawnAt(path: String, create: Boolean = false, target: String = "", host: String = "") =
        JSONObject().put("type", "spawn_at").put("path", path).put("create", create)
            .apply { if (target.isNotEmpty()) put("target", target) }
            .apply { if (host.isNotEmpty()) put("host_name", host) }
            .toString()

    // SSH host registry (Settings → Hosts). The server persists these and broadcasts
    // an updated host_list after every put/delete.
    fun hostsList() = JSONObject().put("type", "hosts").toString()
    fun hostPut(h: Host) =
        JSONObject().put("type", "host_put").put(
            "host",
            JSONObject().put("name", h.name).put("address", h.address)
                .put("user", h.user).put("port", h.port)
                .put("key_file", h.keyFile).put("claude_bin", h.claudeBin),
        ).toString()
    fun hostDelete(name: String) = JSONObject().put("type", "host_delete").put("name", name).toString()
}

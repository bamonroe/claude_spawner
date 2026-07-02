package com.bam.spawner.net

import org.json.JSONArray
import org.json.JSONObject

/**
 * Server -> app messages (see docs/protocol.md). Parsed from JSON with the
 * built-in org.json to avoid a serialization dependency.
 */
sealed interface ServerMsg {
    data class HelloOk(val serverVersion: String) : ServerMsg
    data class Say(val text: String) : ServerMsg
    data class Transcript(val text: String, val final: Boolean) : ServerMsg
    data class Pending(val text: String) : ServerMsg // live hands-free draft buffer
    data class Calibration(val text: String) : ServerMsg // what the detection model heard for a sample
    data class Activity(val text: String) : ServerMsg // live "Claude is thinking / editing X" indicator
    data class Files(val files: List<String>) : ServerMsg // files changed this turn
    data class Dialog(val state: String, val prompt: String) : ServerMsg
    data class Attached(val name: String) : ServerMsg
    data object Detached : ServerMsg
    data class Output(val name: String, val text: String) : ServerMsg
    data class History(val name: String, val messages: List<HistMsg>, val more: Boolean) : ServerMsg
    data class ReadLast(val count: Int) : ServerMsg
    data class SessionList(val sessions: List<SessionInfo>) : ServerMsg
    data class Err(val code: String, val message: String) : ServerMsg
    data object StopSpeaking : ServerMsg
    data class Listing(val path: String, val parent: String, val entries: List<BrowseEntry>) : ServerMsg
    data class Unknown(val type: String) : ServerMsg

    companion object {
        fun parse(raw: String): ServerMsg {
            val o = JSONObject(raw)
            return when (o.optString("type")) {
                "hello_ok" -> HelloOk(o.optString("server_version"))
                "say" -> Say(o.optString("text"))
                "transcript" -> Transcript(o.optString("text"), o.optBoolean("final", true))
                "pending" -> Pending(o.optString("text"))
                "calibration" -> Calibration(o.optString("text"))
                "activity" -> Activity(o.optString("text"))
                "files" -> Files(readStrings(o.optJSONArray("files")))
                "dialog" -> Dialog(o.optString("state"), o.optString("prompt"))
                "attached" -> Attached(o.optString("name"))
                "detached" -> Detached
                "output" -> Output(o.optString("name"), o.optString("text"))
                "history" -> History(o.optString("name"), readHist(o.optJSONArray("messages")), o.optBoolean("more"))
                "read_last" -> ReadLast(o.optInt("count", 1))
                "session_list" -> SessionList(readSessions(o.optJSONArray("sessions")))
                "error" -> Err(o.optString("code"), o.optString("message"))
                "stop_speaking" -> StopSpeaking
                "listing" -> Listing(o.optString("path"), o.optString("parent"), readEntries(o.optJSONArray("entries")))
                else -> Unknown(o.optString("type"))
            }
        }

        private fun readSessions(arr: JSONArray?): List<SessionInfo> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val s = arr.getJSONObject(it)
                SessionInfo(s.optString("name"), s.optString("dir"))
            }
        }

        private fun readHist(arr: JSONArray?): List<HistMsg> {
            if (arr == null) return emptyList()
            return (0 until arr.length()).map {
                val m = arr.getJSONObject(it)
                HistMsg(m.optInt("index", -1), m.optString("role"), m.optString("text"))
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
    }
}

/** One past message from a session's server-served history. */
data class HistMsg(val index: Int, val role: String, val text: String)

/** A session as listed for the sidebar. */
data class SessionInfo(val name: String, val dir: String)

/** A directory in the "new session" browser. */
data class BrowseEntry(val name: String, val path: String, val repo: Boolean)

/** Per-connection preferences sent in the hello handshake. */
data class HelloConfig(
    val endToken: String,
    val sttMode: String,
    val sttModel: String,
    val aliases: Map<String, String>,
    val whisperUrl: String,
)

/** app -> server message builders. */
object Outbound {
    fun hello(token: String, clientId: String, cfg: HelloConfig) =
        JSONObject().put("type", "hello").put("token", token).put("client_id", clientId)
            .put("end_token", cfg.endToken).put("stt_mode", cfg.sttMode).put("stt_model", cfg.sttModel)
            .put("aliases", JSONObject(cfg.aliases)).put("whisper_url", cfg.whisperUrl)
            .toString()
    fun utterance(text: String) = JSONObject().put("type", "utterance").put("text", text).toString()
    fun wake(codec: String, handsFree: Boolean = false, calibrate: Boolean = false) =
        JSONObject().put("type", "wake").put("codec", codec)
            .put("hands_free", handsFree).put("calibrate", calibrate).toString()
    fun audioEnd() = JSONObject().put("type", "audio_end").toString()
    fun commit() = JSONObject().put("type", "commit").toString() // silence-timeout commit
    fun attach(name: String) = JSONObject().put("type", "attach").put("name", name).toString()
    fun detach() = JSONObject().put("type", "detach").toString()
    fun history(name: String, before: Int?, limit: Int = 30): String {
        val o = JSONObject().put("type", "history").put("name", name).put("limit", limit)
        if (before != null) o.put("before", before)
        return o.toString()
    }
    fun listSessions() = JSONObject().put("type", "list_sessions").toString()
    fun rename(name: String, newName: String) =
        JSONObject().put("type", "rename").put("name", name).put("new_name", newName).toString()
    fun delete(name: String) = JSONObject().put("type", "delete").put("name", name).toString()
    fun browse(path: String) = JSONObject().put("type", "browse").put("path", path).toString()
    fun spawnAt(path: String) = JSONObject().put("type", "spawn_at").put("path", path).toString()
    fun ping() = JSONObject().put("type", "ping").toString()
}

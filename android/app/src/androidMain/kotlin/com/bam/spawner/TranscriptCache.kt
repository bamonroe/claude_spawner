package com.bam.spawner

import android.util.Base64
import com.bam.spawner.net.TokenUsage
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File

/**
 * On-disk cache of per-session chat transcripts, so the app can show large chunks
 * of a conversation while offline and skip refetching an unchanged session's
 * history when the user just clicks between sessions.
 *
 * Each session is one JSON file under [dir]. Alongside the messages we store the
 * paging cursor (oldestIndex/hasMore) and the server digest (count+hash) the
 * cached copy corresponds to — the app sends that hash back as `have_hash` (and
 * compares it to the connect-time `digests` sweep) to learn whether anything
 * changed before pulling any message bodies. The hash is opaque: we never
 * recompute it, only round-trip the server's value.
 */
class TranscriptCache(private val dir: File) {
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }

    init { runCatching { dir.mkdirs() } }

    fun load(name: String): CachedSession? {
        val f = fileFor(name)
        if (!f.exists()) return null
        return runCatching { json.decodeFromString<CachedSession>(f.readText()) }.getOrNull()
    }

    fun save(name: String, session: CachedSession) {
        runCatching { fileFor(name).writeText(json.encodeToString(session)) }
    }

    fun remove(name: String) { runCatching { fileFor(name).delete() } }

    // A session name can hold slashes/spaces; base64-url it into a safe, collision-free filename.
    private fun fileFor(name: String): File {
        val safe = Base64.encodeToString(
            name.encodeToByteArray(),
            Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP,
        )
        return File(dir, "$safe.json")
    }
}

/** A persisted session: its (system-note-free) messages, paging cursor, and the
 *  server digest the cache corresponds to. */
@Serializable
data class CachedSession(
    val messages: List<CachedMsg>,
    val oldestIndex: Int,
    val hasMore: Boolean,
    val count: Int,
    val hash: String,
)

@Serializable
data class CachedMsg(
    val role: String,
    val text: String,
    val index: Int,
    val ts: Long,
    val usage: CachedUsage? = null,
)

@Serializable
data class CachedUsage(val input: Int, val output: Int, val cacheWrite: Int, val cacheRead: Int)

// Mapping between the persisted DTOs and the in-memory ChatMessage model.
fun ChatMessage.toCached() = CachedMsg(
    role = role.name, text = text, index = index, ts = ts,
    usage = usage?.let { CachedUsage(it.input, it.output, it.cacheWrite, it.cacheRead) },
)

fun CachedMsg.toChat() = ChatMessage(
    role = runCatching { Role.valueOf(role) }.getOrDefault(Role.SYSTEM),
    text = text, index = index, ts = ts,
    usage = usage?.let { TokenUsage(it.input, it.output, it.cacheWrite, it.cacheRead) },
)

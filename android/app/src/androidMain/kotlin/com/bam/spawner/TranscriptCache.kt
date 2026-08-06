package com.bam.spawner

import android.util.Base64
import com.bam.spawner.net.TokenUsage
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext

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
 *
 * **Memory is authoritative; the disk catches up.** No entry point here ever does
 * file I/O on the calling thread: [save] publishes into memory and hands the JSON
 * encode + write to [scope] (coalescing repeat saves of the same session so a busy
 * stream writes once per flush), and [load] is a suspend function that reads on
 * [Dispatchers.IO] — [peek] is the non-blocking memory hit callers use on the UI
 * thread. That makes "never block a frame on the transcript cache" an invariant of
 * this class rather than a rule its callers have to remember.
 */
class TranscriptCache(private val dir: File, private val scope: CoroutineScope) {
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    private val mem = ConcurrentHashMap<String, CachedSession>()
    private val dirty = ConcurrentHashMap<String, CachedSession>()
    private val flushing = AtomicBoolean(false)

    init { runCatching { dir.mkdirs() } }

    /** The in-memory copy, if this session has been loaded or saved already.
     *  Never touches disk — safe to call on the main thread. */
    fun peek(name: String): CachedSession? = mem[name]

    /** Memory hit, else a disk read on [Dispatchers.IO]. */
    suspend fun load(name: String): CachedSession? {
        mem[name]?.let { return it }
        val c = withContext(Dispatchers.IO) {
            val f = fileFor(name)
            if (!f.exists()) null
            else runCatching { json.decodeFromString<CachedSession>(f.readText()) }.getOrNull()
        } ?: return null
        return mem.putIfAbsent(name, c) ?: c
    }

    /** Publishes to memory immediately and queues the write; returns at once. */
    fun save(name: String, session: CachedSession) {
        mem[name] = session
        dirty[name] = session
        flush()
    }

    fun remove(name: String) {
        mem.remove(name)
        dirty.remove(name)
        scope.launch(Dispatchers.IO) { runCatching { fileFor(name).delete() } }
    }

    // One writer at a time, draining whatever is dirty when it gets there — so N
    // saves of the same session while a write is in flight collapse into one more write.
    private fun flush() {
        if (!flushing.compareAndSet(false, true)) return
        scope.launch(Dispatchers.IO) {
            try {
                while (true) {
                    val name = dirty.keys.firstOrNull() ?: break
                    val s = dirty.remove(name) ?: continue
                    runCatching { fileFor(name).writeText(json.encodeToString(s)) }
                }
            } finally {
                flushing.set(false)
                if (dirty.isNotEmpty()) flush() // a save that landed during the drain
            }
        }
    }

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
    val id: String = "", // durable backend id; default keeps pre-id cache files loadable
)

@Serializable
data class CachedUsage(val input: Int, val output: Int, val cacheWrite: Int, val cacheRead: Int)

// Mapping between the persisted DTOs and the in-memory ChatMessage model.
fun ChatMessage.toCached() = CachedMsg(
    role = role.name, text = text, index = index, ts = ts,
    usage = usage?.let { CachedUsage(it.input, it.output, it.cacheWrite, it.cacheRead) },
    id = id,
)

fun CachedMsg.toChat() = ChatMessage(
    role = runCatching { Role.valueOf(role) }.getOrDefault(Role.SYSTEM),
    text = text, index = index, ts = ts,
    usage = usage?.let { TokenUsage(it.input, it.output, it.cacheWrite, it.cacheRead) },
    id = id,
)

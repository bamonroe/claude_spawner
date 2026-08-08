package com.bam.spawner

import android.util.Base64
import com.bam.spawner.net.TokenUsage
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File
import java.util.Collections
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicLong
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
 *
 * **The warm set is pre-loaded and bounded.** Construction kicks off a background
 * pre-warm that decodes the most recently used cache files into memory, so the
 * FIRST switch to a session after app launch gets a [peek] hit and renders its
 * chat in the same frame instead of a blank list waiting on disk. The in-memory
 * map is an LRU capped at [WARM_CAP]: sessions the user actually flips between
 * stay warm, cold ones fall out, and an evicted entry is simply a [load] (disk)
 * away — the file is the durable copy either way.
 */
class TranscriptCache(private val dir: File, private val scope: CoroutineScope) {
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }
    // Access-ordered LRU behind a synchronized wrapper: even reads reorder entries,
    // so every touch must lock. Uncontended locks are nanoseconds; the values are
    // references — the cap bounds decoded-transcript memory, not correctness.
    private val mem: MutableMap<String, CachedSession> = Collections.synchronizedMap(
        object : LinkedHashMap<String, CachedSession>(16, 0.75f, true) {
            override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, CachedSession>) =
                size > WARM_CAP
        },
    )
    private val dirty = ConcurrentHashMap<String, CachedSession>()
    private val flushing = AtomicBoolean(false)
    private val lastSweep = AtomicLong(0)

    init {
        runCatching { dir.mkdirs() }
        prewarm()
    }

    private companion object {
        // How long a cached transcript survives after its session stops appearing in
        // a server's list. Long enough to span "I haven't opened that machine in a
        // while", short enough that deleted sessions don't accumulate for years.
        const val RETENTION_MS = 14L * 24 * 60 * 60 * 1000
        // Don't re-walk the cache dir more than this often.
        const val SWEEP_INTERVAL_MS = 60L * 60 * 1000
        // The LRU cap on decoded transcripts held in memory, and how many of the
        // most recently used files the startup pre-warm decodes into it. Prewarm
        // stays under the cap so it can't evict a session loaded on demand while
        // it runs.
        const val WARM_CAP = 32
        const val PREWARM_LIMIT = 16
    }

    // Decode the most recently used cache files into memory in the background.
    // mtime is the recency signal ([retainOnly] refreshes it for every live
    // session, and a save rewrites the file), oldest-first insertion leaves the
    // most recent entries freshest in the LRU, and putIfAbsent keeps any entry a
    // faster on-demand load/save already published.
    private fun prewarm() {
        scope.launch(Dispatchers.IO) {
            val files = runCatching { dir.listFiles() }.getOrNull() ?: return@launch
            files.sortedByDescending { it.lastModified() }
                .take(PREWARM_LIMIT)
                .asReversed()
                .forEach { f ->
                    val id = idOf(f) ?: return@forEach
                    decode(f)?.let { mem.putIfAbsent(id, it) }
                }
        }
    }

    /** The in-memory copy, if this session has been loaded or saved already.
     *  Never touches disk — safe to call on the main thread. */
    fun peek(name: String): CachedSession? = mem[name]

    /** Memory hit, else a disk read on [Dispatchers.IO]. */
    suspend fun load(name: String): CachedSession? {
        mem[name]?.let { return it }
        val c = withContext(Dispatchers.IO) { decode(fileFor(name)) } ?: return null
        return mem.putIfAbsent(name, c) ?: c
    }

    // Read + parse one cache file; null for a missing or corrupt file. IO-thread only.
    private fun decode(f: File): CachedSession? {
        if (!f.exists()) return null
        return runCatching { json.decodeFromString<CachedSession>(f.readText()) }.getOrNull()
    }

    /** Publishes to memory immediately and queues the write; returns at once. */
    fun save(name: String, session: CachedSession) {
        mem[name] = session
        dirty[name] = session
        flush()
    }

    /**
     * Re-key a cached transcript from [old] to [new] — a `clear` rotation, whose
     * rendered log is byte-identical (the retired id is only appended to the chain).
     * Moves the memory + pending-write entries and renames the file, so the cached
     * chat survives the rotation instead of being deleted and refetched.
     */
    fun rename(old: String, new: String) {
        if (old == new) return
        mem.remove(old)?.let { mem[new] = it }
        dirty.remove(old)?.let { dirty[new] = it }
        scope.launch(Dispatchers.IO) {
            runCatching {
                val from = fileFor(old)
                val to = fileFor(new)
                if (from.exists()) { to.delete(); if (!from.renameTo(to)) from.delete() }
            }
        }
    }

    fun remove(name: String) {
        mem.remove(name)
        dirty.remove(name)
        scope.launch(Dispatchers.IO) { runCatching { fileFor(name).delete() } }
    }

    /**
     * Drop cached transcripts for sessions that no longer exist. Without this the
     * only deletion path is an explicit [remove], so a session deleted server-side
     * left its transcript JSON on the device forever — the cache grew monotonically
     * for the life of the install.
     *
     * [known] is **one server's** session list, not global truth: the app talks to
     * several servers, and the same cache dir holds sessions from all of them. So
     * absence is treated as evidence, not proof — a file is deleted only after it
     * has gone unseen for [RETENTION_MS]. Every id in [known] has its file's mtime
     * refreshed, which is what "seen" means here (no extra bookkeeping file, and no
     * reading the entries to find out). A session on a server you simply haven't
     * connected to lately survives as long as you come back within the window.
     *
     * Throttled to [SWEEP_INTERVAL_MS] so a chatty `discovered` stream doesn't
     * re-walk the directory on every frame, and runs entirely on [Dispatchers.IO].
     */
    fun retainOnly(known: Set<String>) {
        val now = System.currentTimeMillis()
        val last = lastSweep.get()
        if (now - last < SWEEP_INTERVAL_MS || !lastSweep.compareAndSet(last, now)) return
        scope.launch(Dispatchers.IO) {
            val files = runCatching { dir.listFiles() }.getOrNull() ?: return@launch
            for (f in files) {
                val id = idOf(f) ?: continue
                if (id in known) {
                    runCatching { f.setLastModified(now) }
                    continue
                }
                if (now - f.lastModified() < RETENTION_MS) continue
                mem.remove(id)
                dirty.remove(id)
                runCatching { f.delete() }
            }
        }
    }

    // The inverse of fileFor: recover the session id a cache file belongs to, or
    // null for anything this class didn't write (so a stray file is left alone).
    private fun idOf(f: File): String? {
        val base = f.name.removeSuffix(".json")
        if (base == f.name) return null
        return runCatching {
            Base64.decode(base, Base64.URL_SAFE or Base64.NO_PADDING or Base64.NO_WRAP).decodeToString()
        }.getOrNull()?.takeIf { it.isNotEmpty() }
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

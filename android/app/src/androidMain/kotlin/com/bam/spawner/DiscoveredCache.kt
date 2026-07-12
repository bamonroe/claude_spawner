package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.io.File

/**
 * On-disk cache of the discovered session list, so the sidebar can show the last
 * known sessions on a fresh launch before (or without) a server connection. The
 * per-session chat bodies are cached separately by [TranscriptCache]; this just
 * holds the list of what sessions exist so the user has something to browse and
 * tap into while offline. It's refreshed wholesale on every connect-time
 * `Discovered` sweep.
 *
 * Stored as one JSON file. `DiscoveredInfo` lives in commonMain and isn't
 * serializable, so we mirror it with a DTO here (same pattern as [CachedSession]).
 */
class DiscoveredCache(private val file: File) {
    private val json = Json { ignoreUnknownKeys = true; encodeDefaults = true }

    fun load(): List<DiscoveredInfo> {
        if (!file.exists()) return emptyList()
        return runCatching {
            json.decodeFromString<List<CachedDiscovered>>(file.readText()).map { it.toInfo() }
        }.getOrDefault(emptyList())
    }

    fun save(sessions: List<DiscoveredInfo>) {
        runCatching { file.writeText(json.encodeToString(sessions.map { it.toCached() })) }
    }
}

@Serializable
data class CachedDiscovered(
    val name: String,
    val dir: String,
    val sessionId: String,
    val lastActive: Long,
    val registered: Boolean,
    val target: String = "",
    val host: String = "",
    val agent: String = "",
    val model: String = "",
)

// A cached session is never live: `active`/`busy` are volatile server-side facts
// that only mean something on a connected sweep, so we drop them and rehydrate as
// false — offline, nothing is running.
private fun CachedDiscovered.toInfo() = DiscoveredInfo(
    name = name, dir = dir, sessionId = sessionId, lastActive = lastActive,
    active = false, registered = registered, busy = false,
    target = target, host = host, agent = agent, model = model,
)

private fun DiscoveredInfo.toCached() = CachedDiscovered(
    name = name, dir = dir, sessionId = sessionId, lastActive = lastActive,
    registered = registered, target = target, host = host, agent = agent, model = model,
)

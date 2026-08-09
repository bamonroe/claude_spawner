package com.bam.spawner

import com.bam.spawner.net.ServerMsg

/**
 * ListingCache is the browse picker's per-connection memory of directories it has
 * already seen. A directory listing is a read-only snapshot fetched with one blocking
 * SSH round trip on the target host, so navigating back up used to blank the screen and
 * re-fetch a directory the picker had just displayed.
 *
 * Both controllers (Android, web) route their browse request/reply pair through here, so
 * the caching rule lives in one place rather than in each screen:
 *
 *  - [remember] records the (host, path, files) a request was sent for, so the reply —
 *    which carries only the resolved path — is filed under the right key;
 *  - [hit] returns a cached listing to paint immediately on a tap. The request still goes
 *    out and its reply overwrites the cached copy when it lands (cache-then-refresh, never
 *    cache-instead-of-fetch, so a directory that changed on disk self-corrects);
 *  - [put] files a reply and evicts the least-recently-used entry past [capacity].
 */
class ListingCache(private val capacity: Int = 32) {
    private val entries = LinkedHashMap<String, ServerMsg.Listing>()

    // Requests in flight, keyed by the path the server echoes back in its reply, so
    // several taps can be outstanding at once and each reply files under its own key.
    private val pending = HashMap<String, String>()

    // The server resolves an empty path to the filesystem root before echoing it.
    private fun norm(path: String) = if (path.isEmpty()) "/" else path

    private fun key(path: String, host: String, files: Boolean) =
        "$host ${norm(path)} ${if (files) "f" else "d"}"

    /** The cached listing for this request, or null if it has never been fetched. */
    fun hit(path: String, host: String, files: Boolean): ServerMsg.Listing? {
        val k = key(path, host, files)
        val v = entries.remove(k) ?: return null
        entries[k] = v // touch: most-recently used
        return v
    }

    /** Records the request a `listing` reply will belong to. */
    fun remember(path: String, host: String, files: Boolean) {
        pending[norm(path)] = key(path, host, files)
    }

    /** Files a reply against the request that asked for it. */
    fun put(msg: ServerMsg.Listing) {
        val k = pending.remove(norm(msg.path)) ?: return
        entries.remove(k)
        entries[k] = msg
        while (entries.size > capacity) {
            entries.remove(entries.keys.first())
        }
    }

    /** Drops everything — the cache is per-connection, not per-app-run. */
    fun clear() {
        entries.clear()
        pending.clear()
    }
}

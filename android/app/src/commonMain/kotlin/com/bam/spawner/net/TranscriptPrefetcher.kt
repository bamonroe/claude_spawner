package com.bam.spawner.net

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlin.time.TimeSource

/**
 * Background transcript prefetcher: while the app is idle, quietly refresh the
 * local transcript cache of the most recently active sessions the user is NOT
 * looking at, so tapping into one renders a warm, current chat instead of paying
 * the history round trip in the switch.
 *
 * It issues plain `history{have_hash}` requests — the same frame a switch sends —
 * so replies flow through the controller's existing `onHistory` path: they land in
 * the id-keyed log map and the disk cache, and (because that path only touches the
 * visible chat when the reply's session is the one on screen) a prefetch can never
 * move the user's view. This class deliberately has no way to reach the current
 * view or focus: its [Host] can only send frames and answer "what's on screen /
 * is a turn streaming".
 *
 * It also owns keeping the staleness hint itself fresh: the server answers `digest`
 * once per request, so it re-sends one on a cadence (and immediately when discovery
 * reports a transcript moved) instead of living off the connect-time snapshot.
 *
 * Scheduling: [kick]ed by discovered-list updates, the digest sweep,
 * each landed history reply, and turn completion. At most [MAX_IN_FLIGHT] requests
 * run at once (that's the throttle — the server additionally coalesces per-session
 * bursts), it pauses entirely while a dictation turn streams or while a
 * user-visible history request is outstanding ([Host.foregroundHistoryActive]), and it only fetches
 * the top [TOP_N] most recently active candidates the sweep gives it a reason to
 * fetch (see [worth]). A fetch
 * whose reply never lands frees its slot after [STUCK_MS].
 */
class TranscriptPrefetcher(
    private val scope: CoroutineScope,
    private val session: SessionSync,
    private val host: Host,
) {
    /** The deliberately narrow seam: send a frame, and read-only view state. */
    interface Host {
        /** Write a frame to the socket (drops if the client is null/closed). */
        fun send(frame: String)
        /** The session id currently on screen ("" for none) — never prefetched. */
        fun viewedId(): String
        /** True while a dictation turn streams; prefetch pauses so it never
         *  competes with live output for the connection or the server's readers. */
        fun turnActive(): Boolean

        /** True while a **user-visible** history request is outstanding — the viewed
         *  session's freshness refresh, its attach top page, or a scroll-back /
         *  reconnect gap-fill page. Prefetch pauses for the same reason it pauses on
         *  a turn: the frames the user is actually waiting on should never share the
         *  connection (or the server's history readers) with speculative work. The
         *  server also lands foreground requests in a reserved lane, but not sending
         *  the competing frames at all is the cheaper half of the fix. */
        fun foregroundHistoryActive(): Boolean
    }

    private var known: List<DiscoveredInfo> = emptyList()
    private val inFlight = mutableSetOf<String>()

    /** Sessions this prefetcher has fetched a page for during this connection —
     *  what lets an attach report a `prefetch-hit` apart from a plain cache hit
     *  left over from an earlier visit. Measurement only; nothing branches on it. */
    private val prefetched = mutableSetOf<String>()
    private var lastDigestAt = TimeSource.Monotonic.markNow()

    init {
        // The staleness hint has to keep up with the connection's lifetime. The
        // server answers `digest` once per request, so a connect-time sweep is a
        // frozen snapshot: any session that GROWS later would never look stale
        // again and its attach would pay the full history round trip. Refresh on
        // a modest cadence (repeat sweeps with no transcript movement are a few
        // stats server-side, and the memoized per-session digests make them cheap).
        scope.launch {
            while (true) {
                delay(DIGEST_REFRESH_MS)
                refreshDigest()
            }
        }
    }

    /** The discovered list refreshed — reprioritize against the new recency order. */
    fun onDiscovered(sessions: List<DiscoveredInfo>) {
        val advanced = sessions.any { d ->
            val prev = known.firstOrNull { it.sessionId == d.sessionId }
            prev == null || d.lastActive > prev.lastActive
        }
        known = sessions
        // Discovery just told us a transcript moved; the held digests are provably
        // behind, so re-sweep now rather than waiting out the cadence.
        if (advanced) refreshDigest()
        kick()
    }

    /** A history reply for [id] landed (page or `unchanged`): free its slot. */
    fun onSynced(id: String) {
        if (inFlight.remove(id)) kick()
    }

    /** Something changed that may unblock work (digest sweep landed, turn ended). */
    fun kick() {
        if (host.turnActive() || host.foregroundHistoryActive()) return
        val viewed = host.viewedId()
        val candidates = known.asSequence()
            .filter { it.registered && it.sessionId.isNotEmpty() }
            .filter { it.sessionId != viewed && it.sessionId !in inFlight }
            .sortedByDescending { it.lastActive }
            .take(TOP_N)
            .map { it to worth(it.sessionId) }
            .filter { it.second != Worth.SKIP }
            // Known-mismatch first: it is a certain win. Unknown sessions the sweep
            // said nothing about come after, still in recency order.
            .sortedBy { it.second.ordinal }
            .map { it.first }
        for (d in candidates) {
            if (inFlight.size >= MAX_IN_FLIGHT) return
            prefetch(d)
        }
    }

    /** Re-request the server's transcript digests, rate-limited to [MIN_DIGEST_GAP_MS]
     *  and paused for the same reasons a prefetch is: the sweep shares the connection
     *  with whatever the user is actually waiting on. Drops harmlessly when the socket
     *  is closed — the next connect sends its own sweep. */
    private fun refreshDigest() {
        if (host.turnActive() || host.foregroundHistoryActive()) return
        if (lastDigestAt.elapsedNow().inWholeMilliseconds < MIN_DIGEST_GAP_MS) return
        lastDigestAt = TimeSource.Monotonic.markNow()
        host.send(Outbound.digest())
    }

    /** How much a session is worth prefetching, best first. */
    private enum class Worth { MISMATCH, UNKNOWN, SKIP }

    /**
     * The sweep is a hint, not the authority. A reported hash that differs from the
     * one we hold is a certain win ([Worth.MISMATCH]); a reported hash that matches
     * means there is provably nothing to fetch ([Worth.SKIP]).
     *
     * A session the sweep said *nothing* about — created after the sweep, its digest
     * read errored or timed out, its host was briefly unreachable — used to be skipped
     * outright, which guaranteed it was cold on first tap. When we hold no transcript
     * for it at all there is nothing to lose by fetching, so it is [Worth.UNKNOWN]:
     * prefetchable, but behind every known mismatch. If we do hold something, leave it
     * to the authoritative per-attach `have_hash` check rather than guessing.
     */
    private fun worth(id: String): Worth {
        val server = session.serverDigest(id) ?: return if (session.heldDigest(id) == null) Worth.UNKNOWN else Worth.SKIP
        return if (session.heldDigest(id)?.second != server.second) Worth.MISMATCH else Worth.SKIP
    }

    /** Did a background prefetch fetch [id] this connection? See [prefetched]. */
    fun didPrefetch(id: String): Boolean = id in prefetched

    private fun prefetch(d: DiscoveredInfo) {
        val id = d.sessionId
        inFlight.add(id)
        prefetched.add(id)
        host.send(Outbound.history(id, d.name, null, haveHash = session.heldDigest(id)?.second ?: "", background = true))
        scope.launch {
            delay(STUCK_MS)
            // Reply never landed (dropped socket, server error): free the slot so
            // the prefetcher isn't wedged; don't re-kick — the next natural event
            // (reconnect, discover) decides whether to retry.
            inFlight.remove(id)
        }
    }

    private companion object {
        /** How many non-focused sessions are worth keeping warm. */
        const val TOP_N = 8
        /** The throttle: concurrent prefetch requests per connection. */
        const val MAX_IN_FLIGHT = 2
        /** When an unanswered fetch stops occupying a slot. */
        const val STUCK_MS = 20_000L
        /** Cadence of the background digest re-sweep. */
        const val DIGEST_REFRESH_MS = 60_000L
        /** Floor between two sweeps, so discovery bursts can't storm the server. */
        const val MIN_DIGEST_GAP_MS = 15_000L
    }
}

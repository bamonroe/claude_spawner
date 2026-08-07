package com.bam.spawner.net

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

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
 * Scheduling: [kick]ed by discovered-list updates, the connect-time digest sweep,
 * each landed history reply, and turn completion. At most [MAX_IN_FLIGHT] requests
 * run at once (that's the throttle — the server additionally coalesces per-session
 * bursts), it pauses entirely while a dictation turn streams, and it only fetches
 * the top [TOP_N] most recently active candidates whose server-sweep hash differs
 * from the locally held one (see [SessionSync.recordServerDigest] — sessions the
 * sweep didn't report are left to the authoritative per-attach check). A fetch
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
    }

    private var known: List<DiscoveredInfo> = emptyList()
    private val inFlight = mutableSetOf<String>()

    /** The discovered list refreshed — reprioritize against the new recency order. */
    fun onDiscovered(sessions: List<DiscoveredInfo>) {
        known = sessions
        kick()
    }

    /** A history reply for [id] landed (page or `unchanged`): free its slot. */
    fun onSynced(id: String) {
        if (inFlight.remove(id)) kick()
    }

    /** Something changed that may unblock work (digest sweep landed, turn ended). */
    fun kick() {
        if (host.turnActive()) return
        val viewed = host.viewedId()
        val candidates = known.asSequence()
            .filter { it.registered && it.sessionId.isNotEmpty() }
            .filter { it.sessionId != viewed && it.sessionId !in inFlight }
            .sortedByDescending { it.lastActive }
            .take(TOP_N)
            .filter { stale(it.sessionId) }
        for (d in candidates) {
            if (inFlight.size >= MAX_IN_FLIGHT) return
            prefetch(d)
        }
    }

    /** Fetch only what the server sweep says we don't already hold: no sweep row →
     *  no opinion → leave it to the per-attach check (never burn a fetch on it). */
    private fun stale(id: String): Boolean {
        val server = session.serverDigest(id) ?: return false
        return session.heldDigest(id)?.second != server.second
    }

    private fun prefetch(d: DiscoveredInfo) {
        val id = d.sessionId
        inFlight.add(id)
        host.send(Outbound.history(id, d.name, null, haveHash = session.heldDigest(id)?.second ?: ""))
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
    }
}

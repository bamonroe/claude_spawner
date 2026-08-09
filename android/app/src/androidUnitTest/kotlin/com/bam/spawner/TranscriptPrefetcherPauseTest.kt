package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionSync
import com.bam.spawner.net.TranscriptPrefetcher
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * The prefetcher's pause seam: speculative warm-cache fetches must not go out while
 * the user is waiting on something — a dictation turn, or a foreground history
 * request (the viewed session's refresh/attach page, a scroll-back, a gap-fill page).
 * The server also serves foreground history in a reserved lane; this is the client
 * half, which keeps the competing frames off the wire entirely.
 */
class TranscriptPrefetcherPauseTest {

    private class FakeSessionHost : SessionSync.Host {
        override fun send(frame: String) {}
        override fun discovered() = emptyList<DiscoveredInfo>()
        override fun attachedId() = ""
        override fun attachedName(): String? = null
        override fun attachedAgent() = ""
        override fun attachedModel() = ""
        override fun loadAttachHistory() = emptyList<String>()
        override fun saveAttachHistory(ids: List<String>) {}
    }

    private class FakeHost : TranscriptPrefetcher.Host {
        val sent = mutableListOf<String>()
        var turn = false
        var foreground = false
        override fun send(frame: String) { sent += frame }
        override fun viewedId() = ""
        override fun turnActive() = turn
        override fun foregroundHistoryActive() = foreground
    }

    private fun info(id: String) = DiscoveredInfo(
        name = id, dir = "/$id", sessionId = id, lastActive = 1, active = false, registered = true,
    )

    /** A session the server sweep says has moved on: a prefetch candidate. */
    private fun syncWithStaleSession(id: String): SessionSync =
        SessionSync(FakeSessionHost()).apply {
            recordSynced(id, 2, "held-hash")
            recordServerDigest(id, 4, "server-hash")
        }

    @Test
    fun holdsOffWhileForegroundHistoryIsOutstanding() {
        val session = syncWithStaleSession("s1")
        val host = FakeHost()
        val p = TranscriptPrefetcher(CoroutineScope(Job()), session, host)

        host.foreground = true
        p.onDiscovered(listOf(info("s1")))
        assertTrue(host.sent.isEmpty(), "prefetched while a foreground history request was in flight")

        // …and resumes once that foreground reply lands.
        host.foreground = false
        p.kick()
        assertEquals(1, host.sent.size, "prefetch did not resume after the foreground reply")
        assertTrue(host.sent[0].contains("\"background\":true"), "prefetch frame lost its background flag")
    }

    /** The pre-existing turn pause still holds — the new predicate is additive. */
    @Test
    fun stillHoldsOffDuringATurn() {
        val session = syncWithStaleSession("s1")
        val host = FakeHost()
        val p = TranscriptPrefetcher(CoroutineScope(Job()), session, host)

        host.turn = true
        p.onDiscovered(listOf(info("s1")))
        assertTrue(host.sent.isEmpty(), "prefetched during a live turn")
    }

    /** SessionSync's own marker: set by a fresh-history request, cleared by its reply. */
    @Test
    fun freshHistoryPendingTracksTheOutstandingRefresh() {
        val session = SessionSync(FakeSessionHost())
        assertTrue(!session.freshHistoryPending())
        session.requestFreshHistory("s1", "s1")
        assertTrue(session.freshHistoryPending(), "a fresh-history request left no pending marker")
        session.recordSynced("s1", 2, "h")
        assertTrue(!session.freshHistoryPending(), "the reply did not clear the pending marker")
        // A dropped socket clears it too, so the prefetcher can't stay paused forever.
        session.requestFreshHistory("s2", "s2")
        session.clearHistoryPending()
        assertTrue(!session.freshHistoryPending(), "disconnect did not clear the pending marker")
    }
}

package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionSync
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * The attach history (MRU): the one ordered most-recently-attached list that both the
 * right-edge swap gesture ([SessionSync.swapTarget]) and the radial palette's ring
 * ([SessionSync.attachHistory]) read. These pin the four properties the ring depends on —
 * recency order, de-dup, dead sessions dropping out, and surviving a restart via the
 * persisted ids.
 */
class SessionSyncAttachHistoryTest {

    /** A host whose focus and discovery are settable, with an in-memory "prefs" store. */
    private class FakeHost(var persisted: List<String> = emptyList()) : SessionSync.Host {
        var sessions = mutableListOf<DiscoveredInfo>()
        var focused: String = ""
        val sent = mutableListOf<String>()
        override fun send(frame: String) { sent += frame }
        override fun discovered() = sessions.toList()
        override fun attachedId() = focused
        override fun attachedName() = if (focused.isEmpty()) null else focused
        override fun attachedAgent() = ""
        override fun attachedModel() = ""
        override fun loadAttachHistory() = persisted
        override fun saveAttachHistory(ids: List<String>) { persisted = ids }
    }

    private fun info(id: String, lastActive: Long = 0) =
        DiscoveredInfo(name = id, dir = "/$id", sessionId = id, lastActive = lastActive,
            active = false, registered = true)

    private fun hostWith(vararg ids: String) = FakeHost().apply {
        sessions = ids.map { info(it) }.toMutableList()
    }

    /** Attach to each id in turn, as the controllers do (remember the outgoing focus first). */
    private fun SessionSync.attach(host: FakeHost, id: String) {
        rememberPreviousIfSwitching(id)
        host.focused = id
    }

    @Test
    fun ringIsInAttachRecencyOrderNewestFirst() {
        val host = hostWith("a", "b", "c")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b"); sync.attach(host, "c")
        assertEquals(listOf("b", "a"), sync.attachHistory().map { it.sessionId })
    }

    @Test
    fun reAttachingMovesASessionBackToTheFrontWithoutDuplicating() {
        val host = hostWith("a", "b", "c")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b"); sync.attach(host, "c")
        sync.attach(host, "a"); sync.attach(host, "b")
        assertEquals(listOf("a", "c"), sync.attachHistory().map { it.sessionId })
    }

    @Test
    fun theCurrentSessionIsNeverAringSlot() {
        val host = hostWith("a", "b")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b")
        assertTrue(sync.attachHistory().none { it.sessionId == "b" })
    }

    @Test
    fun deadSessionsDropOutOfTheRing() {
        val host = hostWith("a", "b", "c")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b"); sync.attach(host, "c")
        host.sessions.removeAll { it.sessionId == "a" }
        assertEquals(listOf("b"), sync.attachHistory().map { it.sessionId })
    }

    @Test
    fun withNoHistoryYetTheRingFallsBackToLastActiveOrder() {
        val host = FakeHost().apply {
            sessions = mutableListOf(info("old", 10), info("new", 30), info("mid", 20))
        }
        assertEquals(listOf("new", "mid", "old"), SessionSync(host).attachHistory().map { it.sessionId })
    }

    @Test
    fun historyIsCappedAndPersisted() {
        val host = FakeHost()
        val sync = SessionSync(host)
        val ids = (1..SessionSync.ATTACH_HISTORY_CAP + 5).map { "s$it" }
        host.sessions = ids.map { info(it) }.toMutableList()
        ids.forEach { sync.attach(host, it) }
        assertEquals(SessionSync.ATTACH_HISTORY_CAP, host.persisted.size)
        assertEquals("s${ids.size - 1}", host.persisted.first())
    }

    @Test
    fun aRestartRestoresTheRingFromPersistedIds() {
        val host = FakeHost(persisted = listOf("b", "a")).apply {
            sessions = mutableListOf(info("a"), info("b"), info("c"))
        }
        assertEquals(listOf("b", "a", "c"), SessionSync(host).attachHistory().map { it.sessionId })
    }

    @Test
    fun swapTargetsTheMostRecentlyAttachedOtherSession() {
        val host = hostWith("a", "b", "c")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b"); sync.attach(host, "c")
        val t = sync.swapTarget()
        assertTrue(t is SessionSync.SwapTarget.Focus && t.session.sessionId == "b")
    }

    @Test
    fun swapReportsGoneAndForgetsAVanishedTarget() {
        val host = hostWith("a", "b", "c")
        val sync = SessionSync(host)
        sync.attach(host, "a"); sync.attach(host, "b"); sync.attach(host, "c")
        host.sessions.removeAll { it.sessionId == "b" }
        assertEquals(SessionSync.SwapTarget.Gone, sync.swapTarget())
        // Forgotten: the next swap falls through to the one behind it.
        val next = sync.swapTarget()
        assertTrue(next is SessionSync.SwapTarget.Focus && next.session.sessionId == "a")
    }

    @Test
    fun withNoHistoryAtAllSwapFallsBackToTheServer() {
        val host = hostWith("a")
        assertEquals(SessionSync.SwapTarget.Server, SessionSync(host).swapTarget())
    }
}

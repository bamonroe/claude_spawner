package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionSync
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

/**
 * The shared history-page merge both clients now run (Android and web), so a regression on
 * either side fails here once rather than drifting into two behaviors.
 */
class SessionSyncMergeHistoryTest {
    private fun sync() = SessionSync(object : SessionSync.Host {
        override fun send(frame: String) {}
        override fun discovered(): List<DiscoveredInfo> = emptyList()
        override fun attachedId() = ""
        override fun attachedName(): String? = null
        override fun attachedAgent() = ""
        override fun attachedModel() = ""
    })

    // ts is 1-based: ts == 0 means "no transcript timestamp" and is carried forward from the
    // preceding row by orderByTimestamp, which would make index 0 sort last here.
    private fun hist(index: Int, text: String, ts: Long = index + 1L, role: Role = Role.CLAUDE) =
        ChatMessage(role, text, index, ts = ts)

    private fun live(text: String, ts: Long, role: Role = Role.CLAUDE) =
        ChatMessage(role, text, -1, ts = ts)

    @Test
    fun topPageKeepsIndexedRowsItDoesNotCover() {
        val existing = listOf(hist(0, "a"), hist(1, "b"))
        val page = listOf(hist(1, "b"), hist(2, "c"))
        val m = sync().mergeHistory(existing, page, loadOlder = false, hasMore = true)
        assertEquals(listOf("a", "b", "c"), m.messages.map { it.text })
        assertEquals(1, m.oldest)
        assertNull(m.gapTarget)
    }

    @Test
    fun topPageDropsTheLiveCopyOfARowItNowServes() {
        val existing = listOf(live("reply", ts = 5))
        val page = listOf(hist(0, "reply", ts = 5))
        val m = sync().mergeHistory(existing, page, loadOlder = false, hasMore = false)
        assertEquals(1, m.messages.size)
        assertEquals(0, m.messages[0].index)
    }

    @Test
    fun anEmptyTopPagePreservesTheWholeTranscript() {
        // Antigravity-style backend: pages are always empty; a naked replace wiped the log.
        val existing = listOf(live("hi", ts = 1), live("there", ts = 2))
        val m = sync().mergeHistory(existing, emptyList(), loadOlder = false, hasMore = false)
        assertEquals(listOf("hi", "there"), m.messages.map { it.text })
        assertNull(m.oldest)
    }

    @Test
    fun aSurvivingLiveRowLandsInItsChronologicalSlot() {
        val existing = listOf(live("ack", ts = 5))
        val page = listOf(hist(0, "old", ts = 1), hist(1, "new", ts = 9))
        val m = sync().mergeHistory(existing, page, loadOlder = false, hasMore = false)
        assertEquals(listOf("old", "ack", "new"), m.messages.map { it.text })
    }

    @Test
    fun loadOlderPrependsAndLeavesLiveRowsAlone() {
        val existing = listOf(hist(2, "c", ts = 3), live("still typing", ts = 4))
        val page = listOf(hist(0, "a", ts = 1), hist(1, "b", ts = 2))
        val m = sync().mergeHistory(existing, page, loadOlder = true, hasMore = true)
        assertEquals(listOf("a", "b", "c", "still typing"), m.messages.map { it.text })
        assertEquals(0, m.oldest)
        assertNull(m.gapTarget) // gap-fill is a top-page concern only
    }

    @Test
    fun aTopPageStartingAboveWhatWeHeldReportsTheGapWatermark() {
        val existing = listOf(hist(0, "a"), hist(1, "b"))
        val page = listOf(hist(10, "k"), hist(11, "l"))
        val m = sync().mergeHistory(existing, page, loadOlder = false, hasMore = true)
        assertEquals(1, m.gapTarget)
    }

    @Test
    fun aContiguousTopPageReportsNoGap() {
        val existing = listOf(hist(0, "a"), hist(1, "b"))
        val page = listOf(hist(2, "c"))
        val m = sync().mergeHistory(existing, page, loadOlder = false, hasMore = true)
        assertNull(m.gapTarget)
    }
}

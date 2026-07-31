package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionSync
import kotlin.test.Test
import kotlin.test.assertEquals

/**
 * The shared chat de-dup ([SessionSync.dedupe]) — the one reconciliation point both
 * controllers run on the live add path AND on a history merge. These pin the hands-free
 * duplicate-user-bubble fix: one utterance streams a live draft/echo row and then lands
 * the committed `transcript` as a SECOND identical live row (index==-1 both), so it must
 * collapse to one — without swallowing genuinely distinct rows, a legitimate repeat, or
 * the streamed reply segments.
 */
class SessionSyncDedupeTest {

    private fun sync() = SessionSync(object : SessionSync.Host {
        override fun send(frame: String) {}
        override fun discovered(): List<DiscoveredInfo> = emptyList()
        override fun attachedId(): String = ""
        override fun attachedName(): String? = null
        override fun attachedAgent(): String = ""
        override fun attachedModel(): String = ""
    })

    private fun user(text: String, index: Int = -1) = ChatMessage(Role.USER, text, index = index)
    private fun claude(text: String, index: Int = -1) = ChatMessage(Role.CLAUDE, text, index = index)
    private fun claudeId(text: String, id: String, index: Int = -1) =
        ChatMessage(Role.CLAUDE, text, index = index, id = id)

    // The bug: a hands-free utterance's live draft/echo row and its committed transcript
    // are both live (index==-1) with the same text; they must collapse to ONE user bubble.
    @Test
    fun handsFreeDraftThenCommitCollapseToOneUserRow() {
        val out = sync().dedupe(listOf(user("do the thing"), user("do the thing")))
        assertEquals(listOf(user("do the thing")), out)
    }

    // Whitespace-only differences between the draft and the commit still collapse.
    @Test
    fun adjacentLiveDuplicateCollapsesIgnoringWhitespace() {
        val out = sync().dedupe(listOf(user("hello"), user("  hello ")))
        assertEquals(1, out.size)
    }

    // Two genuinely different utterances are both kept (push-to-talk / distinct dictation).
    @Test
    fun distinctLiveUserRowsAreKept() {
        val out = sync().dedupe(listOf(user("yes"), user("no")))
        assertEquals(2, out.size)
    }

    // A legitimate repeat of the same word across two turns is NOT adjacent — a reply sits
    // between them — so both survive (the server never lets two dictations land back to back).
    @Test
    fun repeatedUserSeparatedByReplyIsKept() {
        val out = sync().dedupe(listOf(user("yes"), claude("ok"), user("yes")))
        assertEquals(3, out.size)
    }

    // Regression: re-saying/typing a word that ALREADY SETTLED into history (an indexed
    // row earlier in the log) must keep the fresh optimistic bubble. A global "text appears
    // in any settled row" fold ate it here — the row vanished the instant it was sent and
    // only reappeared after a hard refresh/reattach (once the server re-indexed it). The
    // fold is positional now: only an adjacent twin collapses, and the stale "yes" isn't one.
    @Test
    fun liveRepeatOfSettledTextIsKept() {
        val out = sync().dedupe(listOf(user("yes", index = 4), claude("ok", index = 5), user("yes")))
        assertEquals(3, out.size)
        assertEquals(-1, out.last().index) // the fresh optimistic row survived
    }

    // Existing behavior preserved: a live row still collapses against a landed indexed row.
    @Test
    fun liveRowCollapsesAgainstIndexedHistoryRow() {
        val out = sync().dedupe(listOf(user("hi", index = 4), user("hi")))
        assertEquals(listOf(user("hi", index = 4)), out)
    }

    // Streamed reply segments (distinct text) are all preserved — only exact repeats fold.
    @Test
    fun distinctStreamedSegmentsPreserved() {
        val out = sync().dedupe(listOf(claude("part one"), claude("part two")))
        assertEquals(2, out.size)
    }

    // The re-index survival case: a stale cached copy (old index) and its freshly
    // re-indexed copy (new index) share a durable id, so they collapse despite the
    // index rewrite that a clear/compress rotation performs — where the index-only
    // rule left both as duplicate bubbles.
    @Test
    fun sameDurableIdCollapsesEvenWhenIndexChanged() {
        val out = sync().dedupe(listOf(
            claudeId("hello", id = "u1", index = 2),
            claudeId("hello", id = "u1", index = 7),
        ))
        assertEquals(1, out.size)
        assertEquals(2, out.first().index) // the first copy wins
    }

    // Two genuinely distinct rows with IDENTICAL text but different durable ids are
    // both kept — the id, not the text, decides identity.
    @Test
    fun distinctDurableIdsAreKept() {
        val out = sync().dedupe(listOf(
            claudeId("hello", id = "u1", index = 0),
            claudeId("hello", id = "u2", index = 1),
        ))
        assertEquals(2, out.size)
    }

    // A live streamed copy (no id/index) still folds into its landed id-bearing row.
    @Test
    fun liveRowCollapsesAgainstIdBearingHistoryRow() {
        val out = sync().dedupe(listOf(claudeId("hi", id = "u1", index = 4), claude("hi")))
        assertEquals(listOf(claudeId("hi", id = "u1", index = 4)), out)
    }
}

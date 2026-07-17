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
        override fun heldContent(name: String): Boolean = false
        override fun dropRows(name: String) {}
    })

    private fun user(text: String, index: Int = -1) = ChatMessage(Role.USER, text, index = index)
    private fun claude(text: String, index: Int = -1) = ChatMessage(Role.CLAUDE, text, index = index)

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
}

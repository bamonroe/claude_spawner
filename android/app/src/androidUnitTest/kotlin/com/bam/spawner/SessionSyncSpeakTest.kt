package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionSync
import kotlin.test.Test
import kotlin.test.assertFalse
import kotlin.test.assertTrue

/**
 * The shared spoken-reply de-dup ([SessionSync.shouldSpeakClose] + [SessionSync.noteSpokenChunk]
 * / [SessionSync.noteTurnStart]) — the voice analog of [SessionSync.dedupe]. These pin the
 * hands-free duplicate-VOICE fix: the display log folds a repeated reply silently, but the
 * closing frame would otherwise be spoken on top of the chunks that already voiced it (or
 * spoken twice when a backend doubles the closing frame). The guard is scoped to one turn, so
 * an identical short reply in the very next turn is still spoken.
 */
class SessionSyncSpeakTest {

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

    private val s = "sess"

    // A normal streamed turn: chunks are voiced, then the closing frame carries the whole
    // reply again — it must NOT be spoken (the chunks already said all of it).
    @Test
    fun streamedTurnCloseIsNotRespoken() {
        val sync = sync()
        sync.noteSpokenChunk(s, "hello ")
        sync.noteSpokenChunk(s, "world")
        assertFalse(sync.shouldSpeakClose(s, "hello world", summaryOnly = false))
    }

    // A buffered reply delivered whole (reconnect) — no chunks were voiced — IS spoken.
    @Test
    fun bufferedReplyWithNoChunksIsSpoken() {
        assertTrue(sync().shouldSpeakClose(s, "the answer", summaryOnly = false))
    }

    // A doubled closing frame (backend emits the final twice) is spoken once, not twice —
    // even for a buffered reply with no chunks in between.
    @Test
    fun doubledCloseIsSpokenOnlyOnce() {
        val sync = sync()
        assertTrue(sync.shouldSpeakClose(s, "done", summaryOnly = false))
        assertFalse(sync.shouldSpeakClose(s, "done", summaryOnly = false))
    }

    // A doubled close after a streamed turn: the first close is suppressed by the chunks,
    // and the duplicate close is suppressed too (this is the reported hands-free bug).
    @Test
    fun doubledCloseAfterStreamIsSuppressed() {
        val sync = sync()
        sync.noteSpokenChunk(s, "streamed reply")
        assertFalse(sync.shouldSpeakClose(s, "streamed reply", summaryOnly = false))
        assertFalse(sync.shouldSpeakClose(s, "streamed reply", summaryOnly = false))
    }

    // An identical short reply in the NEXT user turn is still spoken — the guard is
    // per-turn, cleared by noteTurnStart, never a wall-clock window.
    @Test
    fun identicalReplyInNextTurnIsSpoken() {
        val sync = sync()
        assertTrue(sync.shouldSpeakClose(s, "yes", summaryOnly = false))
        sync.noteTurnStart(s)
        assertTrue(sync.shouldSpeakClose(s, "yes", summaryOnly = false))
    }

    // A new streamed turn also resets the doubled-close guard via the first chunk, so an
    // identical reply that streams in the next turn is spoken (once).
    @Test
    fun identicalStreamedReplyNextTurnIsSpoken() {
        val sync = sync()
        assertTrue(sync.shouldSpeakClose(s, "ok", summaryOnly = false))
        sync.noteSpokenChunk(s, "ok") // next turn's first chunk clears the doubled-close guard
        assertFalse(sync.shouldSpeakClose(s, "ok", summaryOnly = false)) // chunk already voiced it
    }

    // Summary-only mode: only the first N steps stream aloud (the rest beep), so the final
    // result is still spoken even though some chunks were voiced.
    @Test
    fun summaryOnlyFinalIsSpoken() {
        val sync = sync()
        sync.noteSpokenChunk(s, "step one")
        assertTrue(sync.shouldSpeakClose(s, "step one and the rest", summaryOnly = true))
    }

    // Whitespace differences between the concatenated chunks and the closing frame don't
    // defeat the match — the reply is still recognized as already-voiced.
    @Test
    fun whitespaceDifferencesStillMatch() {
        val sync = sync()
        sync.noteSpokenChunk(s, "hello")
        sync.noteSpokenChunk(s, "world")
        assertFalse(sync.shouldSpeakClose(s, "hello world\n", summaryOnly = false))
    }

    // Two different sessions don't cross-suppress each other's replies.
    @Test
    fun distinctSessionsAreIndependent() {
        val sync = sync()
        assertTrue(sync.shouldSpeakClose("a", "done", summaryOnly = false))
        assertTrue(sync.shouldSpeakClose("b", "done", summaryOnly = false))
    }
}

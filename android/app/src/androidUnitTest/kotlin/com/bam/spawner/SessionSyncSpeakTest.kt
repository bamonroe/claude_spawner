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

    // --- Turn-id keyed decisions (the server stamps every output frame with `turn`) ---

    // The id decides, not the text: a backend that reshapes the closing text (Antigravity
    // paragraph restore, opencode part-joins, Claude/Codex multi-message turns) must still
    // have its close suppressed after the same turn's chunks were voiced.
    @Test
    fun idSuppressesCloseWithDifferentTextThanChunks() {
        val sync = sync()
        sync.noteSpokenChunk(s, "flat reply text", turn = "t1")
        assertFalse(sync.shouldSpeakClose(s, "flat reply\n\ntext, reshaped", summaryOnly = false, turn = "t1"))
    }

    // A redelivered close (buffered-final resend / doubled close) with the same id is
    // spoken once, even when the text is identical across turns.
    @Test
    fun idDoubledCloseSpokenOnce() {
        val sync = sync()
        assertTrue(sync.shouldSpeakClose(s, "done", summaryOnly = false, turn = "t1"))
        assertFalse(sync.shouldSpeakClose(s, "done", summaryOnly = false, turn = "t1"))
        // …but the same text under a NEW turn id is a new reply and is spoken.
        assertTrue(sync.shouldSpeakClose(s, "done", summaryOnly = false, turn = "t2"))
    }

    // A buffered whole reply (no chunks reached us) is spoken even with an id present.
    @Test
    fun idBufferedReplyIsSpoken() {
        assertTrue(sync().shouldSpeakClose(s, "the answer", summaryOnly = false, turn = "t1"))
    }

    // Chunks voiced under an OLD turn id don't suppress a different turn's close.
    @Test
    fun idChunksOfOtherTurnDontSuppress() {
        val sync = sync()
        sync.noteSpokenChunk(s, "previous turn prose", turn = "t1")
        assertTrue(sync.shouldSpeakClose(s, "previous turn prose", summaryOnly = false, turn = "t2"))
    }

    // Summary-only mode still speaks the final result even when early chunks of the same
    // turn were voiced (only the first N steps stream aloud there).
    @Test
    fun idSummaryOnlyFinalStillSpoken() {
        val sync = sync()
        sync.noteSpokenChunk(s, "step one", turn = "t1")
        assertTrue(sync.shouldSpeakClose(s, "step one and the rest", summaryOnly = true, turn = "t1"))
    }

    // closeSeen/closeStreamed drive the DISPLAY decision: closeStreamed links a close to
    // its shown chunks by id; closeSeen flags a redelivery — and must be queried before
    // shouldSpeakClose records the id.
    @Test
    fun closeSeenAndStreamedTrackIds() {
        val sync = sync()
        sync.noteChunk(s, "t1")
        assertTrue(sync.closeStreamed(s, "t1"))
        assertFalse(sync.closeStreamed(s, "t2"))
        assertFalse(sync.closeSeen(s, "t1")) // not yet decided on
        sync.shouldSpeakClose(s, "reply", summaryOnly = false, turn = "t1")
        assertTrue(sync.closeSeen(s, "t1")) // now a redelivery
        assertFalse(sync.closeSeen(s, "t2"))
        assertFalse(sync.closeSeen(s, "")) // no id → never "seen" (legacy path decides)
    }

    // noteTurnStart clears the id tracking too: a fresh user turn re-arms everything.
    @Test
    fun idStateClearedByTurnStart() {
        val sync = sync()
        sync.noteChunk(s, "t1")
        sync.shouldSpeakClose(s, "reply", summaryOnly = false, turn = "t1")
        sync.noteTurnStart(s)
        assertFalse(sync.closeStreamed(s, "t1"))
        assertFalse(sync.closeSeen(s, "t1"))
    }
}

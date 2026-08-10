package com.bam.spawner.net

import com.bam.spawner.nowMonotonicMs
import com.bam.spawner.platformLog

/**
 * Measures what a user actually waits for when they tap into a session: the
 * interval from [attachStarted] (the moment focus moves) to the first transcript
 * the client renders, and — the part that decides whether the prefetch work is
 * paying off — *where that transcript came from*.
 *
 * Three sources, and they are not the same win:
 *  - `prefetch-hit`  — rows were already held because a background prefetch
 *                      fetched them this connection. This is the case the
 *                      prefetcher exists to produce.
 *  - `cache-hit`     — rows were held from an earlier visit, no prefetch involved.
 *  - `cold`          — nothing held; the user waited on the network.
 *
 * A warm attach renders at ~0ms and is *confirmed* later by the server's reply,
 * so both instants are reported: `paint` is when pixels appeared, `confirm` is
 * when the server agreed the rows were current (or replaced them). Judging a
 * prefetch fix on paint alone would call every warm attach a success even when
 * the server then shipped a whole new page.
 *
 * Instances are confined to the controller's single thread, like the router state
 * it shadows; no synchronisation.
 */
class AttachLatency(private val log: (String) -> Unit = ::platformLog) {

    private var id: String? = null
    private var startedMs: Long = 0
    private var source: String = "cold"
    private var heldRows: Int = 0
    private var painted = false

    /**
     * Focus moved to [sessionId]. [heldRows] is how many rows the client could
     * paint immediately, and [prefetched] whether a background prefetch is what
     * put them there.
     */
    fun attachStarted(sessionId: String, heldRows: Int, prefetched: Boolean) {
        id = sessionId
        startedMs = nowMonotonicMs()
        this.heldRows = heldRows
        painted = heldRows > 0
        source = when {
            heldRows == 0 -> "cold"
            prefetched -> "prefetch-hit"
            else -> "cache-hit"
        }
        if (painted) log("attach[$sessionId]: $source paint=0ms rows=$heldRows (awaiting server confirmation)")
    }

    /**
     * The server's reply for this attach landed. [outcome] is the reply shape —
     * "unchanged", "delta" or "page" — and [rows] what the client now holds.
     * Only the first reply per attach is reported; later pages are gap-fill, not
     * the thing the user was waiting on.
     */
    fun historyArrived(sessionId: String, outcome: String, rows: Int) {
        val target = id ?: return
        if (sessionId != target) return
        id = null
        val ms = nowMonotonicMs() - startedMs
        val paint = if (painted) "0" else ms.toString()
        log("attach[$sessionId]: $source/$outcome paint=${paint}ms confirm=${ms}ms rows=$rows")
    }
}

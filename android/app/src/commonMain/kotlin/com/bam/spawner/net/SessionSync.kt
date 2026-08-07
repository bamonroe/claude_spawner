package com.bam.spawner.net

import com.bam.spawner.ChatMessage
import com.bam.spawner.orderByTimestamp

/**
 * The single, shared reconciliation point for the app's SESSION + CHAT state — the
 * sibling to [CatalogueSync], hoisted out of the two controllers
 * ([com.bam.spawner.VoiceController] on Android, the web controller on wasmJs) so the
 * platform-agnostic *decision* logic lives in `commonMain` exactly once and the two
 * can't drift.
 *
 * What lives here is the decision logic every session/chat reconcile branch shares:
 *  - **which session am I focused on** ([currentFocusedSession]) and the previous-session
 *    bookkeeping the swap gesture reads ([rememberPrevious], [rememberPreviousIfSwitching],
 *    [rememberPreviousOnAttach], [swapTarget]) — one persisted most-recently-attached list
 *    ([attachHistory]) serves both the swap gesture and the radial palette's ring order;
 *  - **is a transcript current** — every `history` reply feeds [recordSynced], and
 *    [requestFreshHistory] sends that held digest as `have_hash` so the server answers
 *    `unchanged` (no bodies) when nothing moved;
 *  - **how a history page folds into the held transcript** ([mergeHistory]) — the keep/drop
 *    rule, the chronological ordering, the paging cursor and the reconnect gap-fill watermark;
 *  - **index-aware chat de-dup** ([dedupe]) — the *one true* de-dup, keyed on the stable
 *    server `index` (live rows carry `index == -1`; server history rows carry a real index),
 *    falling back to text only for the still-live rows;
 *  - **digest drop on context-reset** ([drop]).
 *
 * All per-session state here is keyed by the stable **session id** (not the display name),
 * so a rename never re-keys anything and a context/backend rotation simply starts fresh under
 * the new id — the old id's entries orphan harmlessly. (This is why there is no `migrate` or
 * attach-rotation guard: those existed only to chase a name-keyed store, which is gone.)
 *
 * What deliberately stays in each controller (behind [Host]) is the platform-specific side
 * effect only: the StateFlow/settings wiring and where the transcript is *stored* (Android
 * is disk-backed, the web is in-memory). The merge itself no longer differs — both clients
 * fold a history page through [mergeHistory] and act on the same decisions.
 *
 * [Host.send] is the platform's socket writer (`client?.send(...)`); a null/closed client
 * simply drops the frame, matching the prior `client?.send(...)` behavior.
 */
class SessionSync(private val host: Host) {
    /** The small platform seam this reconciler reads/writes through. */
    interface Host {
        /** Write a frame to the socket (drops if the client is null/closed). */
        fun send(frame: String)
        /** The current discovered-session list (for focus/swap resolution). */
        fun discovered(): List<DiscoveredInfo>
        /** The attached session's stable id ("" when detached / older server). */
        fun attachedId(): String
        /** The attached session's display name (null when detached). */
        fun attachedName(): String?
        /** The attached session's backend id (for the focus snapshot fallback). */
        fun attachedAgent(): String
        /** The attached session's model alias (for the focus snapshot fallback). */
        fun attachedModel(): String
        /** The persisted attach-history (most-recent first) faulted in at startup.
         *  Defaults to empty so tests and non-persisting hosts need not implement it. */
        fun loadAttachHistory(): List<String> = emptyList()
        /** Persist the attach-history after it changes (most-recent first). */
        fun saveAttachHistory(ids: List<String>) {}
    }

    // The digest of the transcript we currently hold, per session id. Sent as `have_hash`
    // on a (re)attach's history request so the server can answer `unchanged` (no bodies)
    // when nothing moved — the authoritative, bodies-free freshness check.
    private val digestHeld = mutableMapOf<String, Pair<Int, String>>()

    // The server's connect-time digest sweep, per session id. PRIORITIZATION ONLY —
    // the background prefetcher fetches sessions whose server hash differs from what
    // we hold, newest first. Never a freshness short-circuit: it's a snapshot that
    // goes stale for any session we're detached from (no live output invalidates it),
    // so the per-attach `have_hash` → `unchanged` round trip stays authoritative.
    private val serverDigest = mutableMapOf<String, Pair<Int, String>>()

    /** Session id of the outstanding fresh-history request, if any (see [requestFreshHistory]). */
    private var freshPending: String? = null

    // The attach history: session ids in most-recently-*departed* order. The front is the
    // session we were focused on before the current one (the swap gesture's target), and the
    // whole list — with the current focus prepended — is the radial palette's attach-recency
    // ring order. Ids only: names/dirs are re-resolved against discovery on read, so a rename
    // or a dead session never leaves a stale row. Faulted in from persistence on first use so
    // the order survives an app restart.
    private var attachHistory: List<String>? = null

    private fun history(): List<String> =
        attachHistory ?: host.loadAttachHistory().distinct().take(ATTACH_HISTORY_CAP)
            .also { attachHistory = it }

    /** Push [id] to the front of the attach history (de-duped, capped, persisted). */
    private fun pushHistory(id: String) {
        if (id.isBlank()) return
        val next = (listOf(id) + history().filter { it != id }).take(ATTACH_HISTORY_CAP)
        if (next == attachHistory) return
        attachHistory = next
        host.saveAttachHistory(next)
    }

    private fun forgetHistory(id: String) {
        val next = history().filter { it != id }
        if (next == attachHistory) return
        attachHistory = next
        host.saveAttachHistory(next)
    }

    // --- Focus / previous-session tracking -----------------------------------

    /**
     * The session currently in focus as a [DiscoveredInfo], resolved from discovery by the
     * attached stable id, or synthesized from the attached snapshot when discovery hasn't
     * surfaced it yet. Null when nothing is attached (or no id — an older server).
     */
    fun currentFocusedSession(): DiscoveredInfo? {
        val id = host.attachedId()
        val name = host.attachedName() ?: return null
        if (id.isBlank()) return null
        return host.discovered().find { it.sessionId == id } ?: DiscoveredInfo(
            name = name,
            dir = "",
            sessionId = id,
            lastActive = 0,
            active = false,
            registered = true,
            agent = host.attachedAgent(),
            model = host.attachedModel(),
        )
    }

    /** Remember the current focus as the swap target (detach / plain re-focus). */
    fun rememberPrevious() {
        currentFocusedSession()?.let { pushHistory(it.sessionId) }
    }

    /** Remember the current focus only when actually switching to a different id. */
    fun rememberPreviousIfSwitching(newId: String) {
        val current = currentFocusedSession()
        if (current?.sessionId != newId) current?.let { pushHistory(it.sessionId) }
    }

    /** The `attached` branch's rule: remember the outgoing focus when the incoming attach
     *  is a genuinely different session (different id and not the same logical name). */
    fun rememberPreviousOnAttach(name: String, sessionId: String) {
        val sameLogicalSession = host.attachedName() == name
        if (host.attachedId().isNotEmpty() && host.attachedId() != sessionId && !sameLogicalSession) {
            currentFocusedSession()?.let { pushHistory(it.sessionId) }
        }
    }

    /**
     * The attach-history ring order the radial palette shows: the sessions we most recently
     * attached to, newest first, resolved fresh against discovery. The currently focused
     * session is excluded (it's the ring's centre, not a slot), as is anything discovery no
     * longer knows about — an ended/dead session drops out instead of showing a dead slot.
     * Falls back to plain last-active order when we have no history yet (a fresh install),
     * so the ring is never empty when sessions exist.
     */
    fun attachHistory(limit: Int = 8): List<DiscoveredInfo> {
        val current = host.attachedId()
        val discovered = host.discovered()
        val byId = discovered.associateBy { it.sessionId }
        val ordered = history().filter { it != current }.mapNotNull { byId[it] }
        val filler = discovered
            .filter { it.sessionId != current && ordered.none { o -> o.sessionId == it.sessionId } }
            .sortedByDescending { it.lastActive }
        return (ordered + filler).take(limit)
    }

    /** What the swap gesture should do, resolved against the remembered previous session. */
    sealed interface SwapTarget {
        /** No local previous session — fall back to the server-driven swap. */
        object Server : SwapTarget
        /** The previous session no longer exists in discovery. */
        object Gone : SwapTarget
        /** Focus this (freshly re-resolved) session locally. */
        data class Focus(val session: DiscoveredInfo) : SwapTarget
    }

    fun swapTarget(): SwapTarget {
        val target = history().firstOrNull { it != host.attachedId() } ?: return SwapTarget.Server
        val discovered = host.discovered()
        val refreshed = discovered.firstOrNull { it.sessionId == target }
        if (refreshed == null) {
            // Discovery hasn't landed yet: keep the entry and let the server swap decide.
            if (discovered.isEmpty()) return SwapTarget.Server
            forgetHistory(target)
            return SwapTarget.Gone
        }
        return SwapTarget.Focus(refreshed)
    }

    // --- Digest cache + history-freshness decision ---------------------------

    /** The digest our stored transcript corresponds to (for persistence). */
    fun heldDigest(id: String): Pair<Int, String>? = digestHeld[id]

    /** Seed the held digest alone (faulting a cached transcript in from disk). */
    fun recordHeld(id: String, count: Int, hash: String) { digestHeld[id] = count to hash }

    /** A history page/`unchanged` reply confirms the stored transcript now equals the
     *  server's: record its digest so the next attach's `have_hash` check stands. */
    fun recordSynced(id: String, count: Int, hash: String) {
        digestHeld[id] = count to hash
        if (freshPending == id) freshPending = null
    }

    /** One row of the server's `digests` sweep landed: hold it as a prefetch hint. */
    fun recordServerDigest(id: String, count: Int, hash: String) {
        serverDigest[id] = count to hash
    }

    /** The server-sweep digest for a session, if the sweep reported one. */
    fun serverDigest(id: String): Pair<Int, String>? = serverDigest[id]

    /** Drop the held digest for a session (context-reset rotation / cache wipe). */
    fun drop(id: String) {
        digestHeld.remove(id)
        serverDigest.remove(id)
        if (freshPending == id) freshPending = null
    }

    /**
     * A context rotation (clear/compress) retired [oldId] for [newId] — broadcast to every
     * device, not just the one attached. Re-key the attach history in place so the swap
     * gesture and the radial palette keep pointing at the (same, live) session instead of a
     * handle no session owns, and drop the old id's digest/freshness state: its transcript
     * was wiped or summarized, so the next fetch re-establishes both under the new id.
     */
    fun remapId(oldId: String, newId: String) {
        if (oldId.isBlank() || newId.isBlank() || oldId == newId) return
        val next = history().map { if (it == oldId) newId else it }.distinct()
        if (next != attachHistory) {
            attachHistory = next
            host.saveAttachHistory(next)
        }
        drop(oldId)
    }

    /**
     * (Re)attach freshness: always refetch the recent history page so a session that
     * advanced while we viewed another — or while we were detached from it entirely and
     * so never received its live output — isn't left stale. The held hash rides along, so
     * when nothing actually moved the server answers `unchanged` (no bodies): a cheap round
     * trip, not a full transfer, and the authoritative freshness check.
     *
     * We deliberately do NOT short-circuit locally on a cached digest match. `serverDigest`
     * is only a connect-time snapshot; for any session we're detached from we get no live
     * output to invalidate it, so a stale entry can spuriously equal what we hold and make
     * us skip the refetch — silently dropping every message that landed while we were away
     * (the "messages vanish until a hard refresh" bug). The server's `unchanged` reply is
     * the same optimization done correctly, so we defer to it instead.
     */
    fun requestFreshHistory(id: String, name: String) {
        // Idempotent per session: a switch asks twice — optimistically when the sidebar
        // row is tapped, then again when the server's `attached` echo lands — and each
        // costs a server-side digest/transcript parse plus a round trip. Suppress the
        // second while the first is still outstanding. The marker is a single slot keyed
        // by id, so it can never wedge: a request for any *other* session replaces it,
        // and the history/`unchanged` reply (recordSynced) or a cache drop clears it.
        if (freshPending == id) return
        freshPending = id
        val held = digestHeld[id]
        host.send(Outbound.history(id, name, null, haveHash = held?.second ?: ""))
    }

    // --- Chat de-dup ---------------------------------------------------------

    /**
     * The one true chat de-dup. A row's identity is its durable backend `id` when it has
     * one, else its stable server `index`; live streamed rows carry neither (`id == ""`,
     * `index == -1`). Three collapses, in order:
     *
     *  1. **By `id`** — a non-empty durable id dedupes FIRST, independent of index. This is
     *     what survives a clear/compress rotation (or a cross-backend re-index): the same
     *     underlying row fetched twice with *different* indexes — a stale cached copy vs. a
     *     freshly re-indexed one — collapses because its id is unchanged, where the old
     *     index-only rule left both as duplicate bubbles.
     *  2. **By `index`** — id-less indexed history rows (Codex/Antigravity backends) keep
     *     the positional collapse, exactly as before.
     *  3. **Adjacent live fold** — drop a live row when its `(role, text)` exactly repeats
     *     the row **immediately before it** (settled or live). This is deliberately
     *     *positional*, not a global "does this text appear in any settled row" test: the
     *     fold must only eat a live row that its neighbour actually replaces, never a fresh
     *     live row that merely happens to repeat some older settled row elsewhere in the log
     *     (typing/saying "yes" again after an earlier "yes" already settled into history —
     *     which a global text fold silently swallowed until a hard refresh). Two things it
     *     legitimately collapses, both adjacent by construction:
     *       - a live streamed row landing on top of its just-settled twin: the live draft
     *         and the indexed/id-bearing history row of the SAME text sort adjacently, so the
     *         live copy folds into the settled one once it lands (the streamed-reply and
     *         typed-echo → history case);
     *       - the hands-free double bubble: an utterance is streamed as a live draft/echo row
     *         and then the committed `transcript` lands the SAME text as a second live row
     *         (both live, so neither id nor index can touch it) — and a backend can likewise
     *         double-emit a reply's closing frame.
     *     Collapsing an *adjacent* identical pair is safe — two genuinely separate messages
     *     can never sit adjacent, because the server refuses a new dictation while a turn is
     *     in flight, so a real repeat always has a reply/ask/error row between the two. Live
     *     rows with no adjacent twin are kept (a turn still streaming, a legitimate repeat).
     */
    /** Records [key] as seen, answering false only when it is a non-empty key we already
     *  hold. An empty key (a live row with no wire identity) is never a duplicate here —
     *  it falls through to the adjacency rule below. */
    private fun MutableSet<String>.addIfKeyed(key: String): Boolean = key.isEmpty() || add(key)

    fun dedupe(messages: List<ChatMessage>): List<ChatMessage> {
        val seenIds = mutableSetOf<String>()
        val seenIndexes = mutableSetOf<Int>()
        val seenLive = mutableSetOf<String>()
        val out = ArrayList<ChatMessage>(messages.size)
        for (m in messages) {
            val keep = when {
                m.id.isNotEmpty() -> seenIds.add(m.id)
                m.index >= 0 -> seenIndexes.add(m.index)
                // A live row with a wire identity (`<turn>:<seq>` off its `output` frame)
                // is deduped by KEY, not adjacency — the server replays a mid-turn attach
                // the frames it missed, and a block replay never sits adjacent to the copies
                // already held. Both checks apply: the key kills a replayed twin anywhere in
                // the log, the adjacency rule still folds a live draft into its settled twin.
                !seenLive.addIfKeyed(m.liveKey) -> false
                out.isNotEmpty() && out.last().role == m.role &&
                    out.last().text.trim() == m.text.trim() -> false
                else -> true
            }
            if (keep) out.add(m)
        }
        return out
    }

    // --- History-page merge --------------------------------------------------

    /**
     * The outcome of folding one server history page into a held transcript: the merged
     * rows plus the paging decisions that follow from them. Pure data — the caller applies
     * it to its own store and performs the platform side effects (persist, refetch).
     */
    data class HistoryMerge(
        /** The merged, chronologically-ordered, de-duplicated transcript. */
        val messages: List<ChatMessage>,
        /** The new oldest-index cursor, or null when the page was empty (leave it alone). */
        val oldest: Int?,
        /** Whether the server says older pages remain. */
        val hasMore: Boolean,
        /**
         * The reconnect gap-fill watermark: the highest index we already held when a *top*
         * page landed that starts above it, leaving a hole. The caller keeps paging older
         * until its cursor reaches this. Null when there is no gap.
         */
        val gapTarget: Int?,
    )

    /**
     * The one true history-page merge, shared by both clients (Android and web) so the two
     * can't drift. Given what we [existing]ly hold and an inbound [page]:
     *
     *  - a **top page** (an attach/reattach refresh, `loadOlder == false`) is the authoritative
     *    tail: existing rows it re-serves are dropped — indexed ones by `index`, live
     *    (`index < 0`) ones by `(role, text)` — but everything it does *not* cover survives.
     *    That preservation is load-bearing: indexed rows from older pages already paged in stay,
     *    as does a live row for a turn still streaming, and a backend with no readable transcript
     *    (Antigravity always answers with an empty page) keeps the only copy of its conversation
     *    instead of being wiped on every reconnect.
     *  - a **load-older page** leaves live rows untouched and only drops indexed rows the page
     *    itself re-serves.
     *  - the result is ordered by timestamp, not concatenated: a surviving live row may be
     *    *older* than the page, and `page + existing` would strand it at the bottom.
     *  - finally [dedupe] collapses anything the two views of the same row share.
     */
    fun mergeHistory(
        existing: List<ChatMessage>,
        page: List<ChatMessage>,
        loadOlder: Boolean,
        hasMore: Boolean,
    ): HistoryMerge {
        val heldMax = existing.mapNotNull { it.index.takeIf { i -> i >= 0 } }.maxOrNull()
        val pageIdx = page.mapNotNull { m -> m.index.takeIf { i -> i >= 0 } }.toSet()
        val pageTexts = if (loadOlder) emptySet() else page.map { it.role to it.text }.toSet()
        val kept = existing.filter {
            (it.index < 0 && (it.role to it.text) !in pageTexts) || (it.index >= 0 && it.index !in pageIdx)
        }
        val pageOldest = page.firstOrNull()?.index
        val gap = if (!loadOlder && heldMax != null && pageOldest != null && pageOldest > heldMax + 1) heldMax else null
        return HistoryMerge(
            messages = dedupe(orderByTimestamp(page + kept)),
            oldest = pageOldest,
            hasMore = hasMore,
            gapTarget = gap,
        )
    }

    // --- Spoken-reply de-dup (hands-free TTS) --------------------------------
    //
    // The *speech* analog of [dedupe]. [dedupe] keeps the displayed log from SHOWING a
    // reply twice; this keeps the voice from SAYING it twice — and they have to agree even
    // though the display self-heals silently while a duplicate speak has already been
    // uttered. The signals differ from the display's:
    //  - a streamed reply is voiced as N chunks (never one full-reply bubble), and its
    //    closing frame carries the whole text AGAIN — re-speaking it duplicates the chunks;
    //  - a backend can emit that closing frame twice, or it can arrive after the old
    //    `streamedSessions` flag was cleared mid-turn — both re-speak a reply the display
    //    dedupe folds by index/adjacency.
    // The server stamps every `output` frame of one turn (chunks + close) with an opaque
    // `turn` id — the authoritative dedup key (see docs/protocol.md, Output path note).
    // Text equality between a chunk and its close is NOT guaranteed (Antigravity restores
    // paragraph breaks, opencode re-joins parts, Claude/Codex's close is only the last
    // message of a multi-message turn), so when an id is present it decides alone; the
    // text comparison survives only as the fallback for a pre-turn-id server ("").
    // A fresh user turn ([noteTurnStart]) clears everything so an identical short reply
    // in the very next turn ("done", "yes") is still spoken — the guard is scoped to one
    // turn, never a wall-clock window.
    private val turnVoiced = mutableMapOf<String, StringBuilder>() // chunk text spoken so far this turn
    private val lastCloseVoiced = mutableMapOf<String, String>()   // last closing reply we decided on
    private val chunkTurn = mutableMapOf<String, String>()         // turn id of the chunks last SHOWN
    private val voicedTurn = mutableMapOf<String, String>()        // turn id of the chunks last SPOKEN
    private val lastCloseTurn = mutableMapOf<String, String>()     // turn id of the last close decided on

    private fun collapseSpace(s: String) = s.trim().replace(whitespace, "")

    /** A fresh user turn began for session [id] (dictation committed / new prompt): reset the
     *  spoken-reply tracking so the next reply — even byte-identical text — is voiced. */
    fun noteTurnStart(id: String) {
        turnVoiced.remove(id)
        lastCloseVoiced.remove(id)
        chunkTurn.remove(id)
        voicedTurn.remove(id)
        lastCloseTurn.remove(id)
    }

    /** A streamed chunk for session [id] was SHOWN (spoken or not — beeped summary chunks and
     *  chunks of a non-current session count too). Lets [closeStreamed] answer "did this
     *  close's turn already stream to us?" for the display's bubble-vs-badge decision. */
    fun noteChunk(id: String, turn: String) {
        if (turn.isNotEmpty()) chunkTurn[id] = turn
    }

    /** Did the closing frame's [turn] already stream chunks to this client? True means
     *  the reply's bubble was built from chunks and the close is just an end marker. */
    fun closeStreamed(id: String, turn: String): Boolean =
        turn.isNotEmpty() && chunkTurn[id] == turn

    /** Is this closing frame a redelivery of a close already handled (buffered-final
     *  resend, doubled close)? Query BEFORE [shouldSpeakClose] — that call records the id. */
    fun closeSeen(id: String, turn: String): Boolean =
        turn.isNotEmpty() && lastCloseTurn[id] == turn

    /** Check-and-record any turn-terminal frame (`ask`, `turn_stopped`, a turn-failed
     *  `error`, the compress `say`) by its [turn] id: true means this terminal was already
     *  presented — a buffered redelivery on reconnect — and the caller should drop it
     *  (don't re-render, don't re-speak). Shares the close registry with
     *  [shouldSpeakClose], since a turn ends in exactly one terminal. A missing id
     *  (pre-turn-id server) returns false so legacy handling decides. */
    fun terminalSeen(id: String, turn: String): Boolean {
        if (turn.isEmpty()) return false
        if (lastCloseTurn[id] == turn) return true
        lastCloseTurn[id] = turn
        return false
    }

    /** A chunk of the streaming reply for session [id] was actually SPOKEN aloud (not beeped).
     *  Accumulates the voiced text so [shouldSpeakClose] can tell the closing frame just
     *  repeats what the chunks already said. The first chunk of a turn (fresh [turn] id,
     *  or accumulator absent when no id) also resets the doubled-close guard, so a new
     *  streamed turn starts clean without a [noteTurnStart]. */
    fun noteSpokenChunk(id: String, text: String, turn: String = "") {
        val newTurn = turn.isNotEmpty() && voicedTurn[id] != turn
        if (turn.isNotEmpty()) voicedTurn[id] = turn
        val sb = turnVoiced[id]
        if (sb == null || newTurn) {
            lastCloseVoiced.remove(id)
            turnVoiced[id] = StringBuilder(text.trim())
        } else {
            sb.append(text.trim())
        }
    }

    /**
     * Should the turn-closing reply [text] for session [id] be spoken aloud? Returns false when
     * the chunks of the same [turn] were already voiced (a normal streamed turn — the id
     * decides, whatever the close's text looks like), or when this close's id was already
     * decided on (a buffered-final redelivery / doubled close). Returns true for a
     * buffered reply delivered whole (reconnect) and for the final result in [summaryOnly]
     * mode (there only the first N steps streamed aloud, so the full result is still spoken —
     * unless that result is byte-identical to what already streamed aloud, i.e. a
     * single-message turn, which would otherwise be voiced twice).
     * With no id (pre-turn-id server) falls back to whitespace-insensitive text equality.
     * Finalizes the turn's voiced state either way, so call it exactly once per closing frame.
     */
    fun shouldSpeakClose(id: String, text: String, summaryOnly: Boolean, turn: String = ""): Boolean {
        val voiced = turnVoiced.remove(id)?.let { collapseSpace(it.toString()) }.orEmpty()
        if (turn.isNotEmpty()) {
            val voicedThisTurn = voicedTurn.remove(id) == turn
            val doubled = lastCloseTurn[id] == turn
            lastCloseTurn[id] = turn
            if (doubled) return false
            // Even in summary-only mode, a single-message turn whose one reply is both the
            // first streamed step (voiced by speakInitialReplies) and the terminal result
            // must not be spoken twice: suppress when the close text is what was voiced.
            if (voicedThisTurn && voiced.isNotEmpty() && voiced == collapseSpace(text)) return false
            if (!summaryOnly && voicedThisTurn) return false
            return true
        }
        val norm = collapseSpace(text)
        val doubled = lastCloseVoiced[id] == norm
        lastCloseVoiced[id] = norm
        if (doubled) return false
        if (voiced.isNotEmpty() && voiced == norm) return false
        return true
    }

    companion object {
        /** How many attached sessions the history remembers — comfortably above the
         *  radial palette's 8 slots, so slots refill as dead sessions drop out. */
        const val ATTACH_HISTORY_CAP = 16

        private val whitespace = Regex("\\s+")
    }
}

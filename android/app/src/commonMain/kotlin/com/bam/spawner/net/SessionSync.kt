package com.bam.spawner.net

import com.bam.spawner.ChatMessage

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
 *    [rememberPreviousOnAttach], [swapTarget]);
 *  - **is a transcript current** — every `history` reply feeds [recordSynced], and
 *    [requestFreshHistory] sends that held digest as `have_hash` so the server answers
 *    `unchanged` (no bodies) when nothing moved;
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
 * effect: the StateFlow/settings wiring and the chat-log *storage* + *merge* strategy, which
 * genuinely differ (Android is disk-backed with a timestamp merge + reconnect gap-fill; the
 * web is in-memory with an index sort). Those talk to this reconciler through [Host] rather
 * than duplicating its decisions.
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
    }

    // The digest of the transcript we currently hold, per session id. Sent as `have_hash`
    // on a (re)attach's history request so the server can answer `unchanged` (no bodies)
    // when nothing moved — the authoritative, bodies-free freshness check.
    private val digestHeld = mutableMapOf<String, Pair<Int, String>>()

    // The session we were focused on before the current one — the swap gesture's target.
    private var previousFocusedSession: DiscoveredInfo? = null

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
        currentFocusedSession()?.let { previousFocusedSession = it }
    }

    /** Remember the current focus only when actually switching to a different id. */
    fun rememberPreviousIfSwitching(newId: String) {
        val current = currentFocusedSession()
        if (current?.sessionId != newId) current?.let { previousFocusedSession = it }
    }

    /** The `attached` branch's rule: remember the outgoing focus when the incoming attach
     *  is a genuinely different session (different id and not the same logical name). */
    fun rememberPreviousOnAttach(name: String, sessionId: String) {
        val sameLogicalSession = host.attachedName() == name
        if (host.attachedId().isNotEmpty() && host.attachedId() != sessionId && !sameLogicalSession) {
            currentFocusedSession()?.let { previousFocusedSession = it }
        }
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
        val target = previousFocusedSession
        if (target == null || target.sessionId.isBlank()) return SwapTarget.Server
        val discovered = host.discovered()
        val refreshed = discovered.firstOrNull { it.sessionId == target.sessionId }
        if (refreshed == null && discovered.isNotEmpty()) {
            previousFocusedSession = null
            return SwapTarget.Gone
        }
        return SwapTarget.Focus(refreshed ?: target)
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
    }

    /** Drop the held digest for a session (context-reset rotation / cache wipe). */
    fun drop(id: String) {
        digestHeld.remove(id)
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
        val held = digestHeld[id]
        host.send(Outbound.history(name, null, haveHash = held?.second ?: ""))
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
     *  3. **Live fold** — drop a live row when its `(role, text)` already appears in a
     *     settled (id- or index-bearing) row: folds the N partial live chunks of a streamed
     *     reply into the one history row once it lands; OR when it exactly repeats the live
     *     row immediately before it. That adjacent-live case is the hands-free double bubble:
     *     the utterance is streamed as a live draft/echo row and then the committed
     *     `transcript` lands the SAME text as a second live row (both live), which neither
     *     id nor index can touch; a backend can likewise double-emit a reply's closing frame.
     *     It is safe to collapse an *adjacent* identical live pair — two genuinely separate
     *     messages can never sit adjacent here, because the server refuses a new dictation
     *     while a turn is in flight, so a real repeat always has a reply/ask/error row
     *     between the two. Live rows with no such match are kept (a turn still streaming).
     */
    fun dedupe(messages: List<ChatMessage>): List<ChatMessage> {
        val settledText = messages
            .filter { it.index >= 0 || it.id.isNotEmpty() }
            .map { it.role to it.text.trim() }
            .toSet()
        val seenIds = mutableSetOf<String>()
        val seenIndexes = mutableSetOf<Int>()
        val out = ArrayList<ChatMessage>(messages.size)
        for (m in messages) {
            val keep = when {
                m.id.isNotEmpty() -> seenIds.add(m.id)
                m.index >= 0 -> seenIndexes.add(m.index)
                settledText.isNotEmpty() && (m.role to m.text.trim()) in settledText -> false
                out.isNotEmpty() && out.last().index < 0 && out.last().id.isEmpty() &&
                    out.last().role == m.role && out.last().text.trim() == m.text.trim() -> false
                else -> true
            }
            if (keep) out.add(m)
        }
        return out
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

    private companion object {
        private val whitespace = Regex("\\s+")
    }
}

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
 *  - **is a transcript current vs. the held digest** — the connect-time `digests` sweep and
 *    every `history` reply feed [noteServerTruth]/[recordSynced], and [requestFreshHistory]
 *    is the one decision that skips a refetch when the held digest still equals the server's;
 *  - **index-aware chat de-dup** ([dedupe]) — the *one true* de-dup, keyed on the stable
 *    server `index` (live rows carry `index == -1`; server history rows carry a real index),
 *    falling back to text only for the still-live rows;
 *  - **digest key migration/drop on rename & context-reset** ([migrate], [drop]).
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
        /** Whether we hold any real (indexed) transcript content for [name]. */
        fun heldContent(name: String): Boolean
        /** Drop a session's platform-held transcript rows + paging cursors (and digests) —
         *  the same wipe `context_reset` performs — when a same-name `session_id` rotation
         *  delivered via `attached` has invalidated the rows we hold under that name. */
        fun dropRows(name: String)
    }

    // The digest caches, per session name. `serverDigest` is the latest truth the server
    // reported (connect-time `digests` sweep + every `history` reply); `digestHeld` is the
    // digest our stored transcript corresponds to. When the two match and we hold content,
    // an (re)attach skips the history fetch entirely.
    private val serverDigest = mutableMapOf<String, Pair<Int, String>>()
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
    fun heldDigest(name: String): Pair<Int, String>? = digestHeld[name]

    /** Seed the held digest alone (faulting a cached transcript in from disk). */
    fun recordHeld(name: String, count: Int, hash: String) { digestHeld[name] = count to hash }

    /** A history page/`unchanged` confirms the stored transcript now equals the server's:
     *  record it as both held and server truth so future freshness checks stand. */
    fun recordSynced(name: String, count: Int, hash: String) {
        digestHeld[name] = count to hash
        serverDigest[name] = count to hash
    }

    /** The connect-time `digests` sweep: the latest server truth per session (bodies-free). */
    fun noteServerTruth(items: List<SessionDigest>) {
        for (d in items) serverDigest[d.name] = d.count to d.hash
    }

    /** A fresh live user/claude line grew this session past our stored digest — forget the
     *  server truth so the next reattach refetches instead of trusting a stale match. */
    fun forgetServerTruth(name: String) { serverDigest.remove(name) }

    /** Drop both digests for a session (context-reset rotation / cache wipe). */
    fun drop(name: String) {
        digestHeld.remove(name)
        serverDigest.remove(name)
    }

    /** Re-key both digests when a session is renamed. */
    fun migrate(old: String, new: String) {
        digestHeld.remove(old)?.let { digestHeld[new] = it }
        serverDigest.remove(old)?.let { serverDigest[new] = it }
    }

    /**
     * (Re)attach freshness decision: refetch the recent history page so a session that
     * advanced while we viewed another isn't left stale — but skip the round trip when the
     * connect-time digest sweep says the server hash still equals what we hold (and we hold
     * content). Otherwise ask for the page, passing the held hash so the server can still
     * answer `unchanged` (no bodies) if nothing moved.
     */
    fun requestFreshHistory(name: String) {
        val held = digestHeld[name]
        val server = serverDigest[name]
        if (held != null && held == server && host.heldContent(name)) return
        host.send(Outbound.history(name, null, haveHash = held?.second ?: ""))
    }

    /**
     * `attached`-path rotation guard. A backend switch (`set_agent`) rotates a session's
     * `session_id` while KEEPING its name and re-emits `attached` (not `context_reset`) — so
     * the rows we still hold under that name are the wiped OLD backend's transcript, and a
     * name-keyed digest match could even make [requestFreshHistory] skip the refetch. Detect
     * exactly that: the incoming attach is for the session we're already attached to (same
     * name) but carries a DIFFERENT, non-empty id than the one we currently hold. When it is,
     * drop the stale rows + digests through [Host.dropRows] so the caller's following
     * [requestFreshHistory] refetches from scratch, mirroring `context_reset`. A normal
     * re-attach — or a swap to a session with the SAME id — drops nothing. Call this BEFORE
     * updating the attached id/name state (it reads the id/name still held). Returns true when
     * a rotation was detected and dropped.
     */
    fun onAttachRotation(name: String, sessionId: String): Boolean {
        if (sessionId.isEmpty()) return false
        if (host.attachedName() != name) return false
        val held = host.attachedId()
        if (held.isEmpty() || held == sessionId) return false
        host.dropRows(name)
        return true
    }

    // --- Chat de-dup ---------------------------------------------------------

    /**
     * The one true chat de-dup, keyed on the stable server `index`. Server history rows
     * carry a real index; live streamed rows carry `index == -1`. Collapse duplicate
     * indexed rows by index, and drop a live row only when its `(role, text)` already
     * appears in an indexed row — the fallback that folds the N partial live chunks of a
     * streamed reply into the one indexed history row once it lands. Live rows with no
     * indexed match are kept (a turn still streaming, not yet persisted).
     */
    fun dedupe(messages: List<ChatMessage>): List<ChatMessage> {
        val indexedText = messages
            .filter { it.index >= 0 }
            .map { it.role to it.text.trim() }
            .toSet()
        val seenIndexes = mutableSetOf<Int>()
        return messages.filter { m ->
            when {
                m.index >= 0 -> seenIndexes.add(m.index)
                indexedText.isNotEmpty() && (m.role to m.text.trim()) in indexedText -> false
                else -> true
            }
        }
    }
}

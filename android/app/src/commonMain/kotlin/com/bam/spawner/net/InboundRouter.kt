package com.bam.spawner.net

import com.bam.spawner.ChatMessage
import com.bam.spawner.Role
import com.bam.spawner.TurnUsageInfo
import com.bam.spawner.nowEpochSeconds
import com.bam.spawner.nowMonotonicMs

/**
 * The single, shared inbound-message router for the per-session CHAT/LOG filing every
 * session-scoped server frame does — the third sibling to [SessionSync] and [CatalogueSync],
 * hoisted out of the controllers so the platform-agnostic *filing* logic (which session a
 * frame belongs to, how its bubble is appended/deduped, when the meter/indicator/ask may be
 * touched) lives in `commonMain` exactly once and the web (wasmJs) and Android controllers
 * can't drift.
 *
 * Both controllers wire to it: the web ([WebAppController]) and Android ([VoiceController]).
 * The web keeps the [Host] defaults for the platform reactions it has no analogue for (the turn
 * watchdog, the voice pill, pocket notifications, spoken notices); Android overrides them.
 *
 * What lives here is the neutral per-session bookkeeping and the filing bodies of the
 * session-scoped frames:
 *  - the in-memory transcript store keyed by the stable **session id** ([logs], [oldest],
 *    [hasMore]) plus the view cursors ([currentId], [loadingOlder]) and the per-turn streamed/
 *    spoken tracking ([streamedSessions], [spokenReplyCounts]);
 *  - the single message-keying rule ([keyOf] = the frame's session id, or the current view
 *    when the frame omits it — an older server);
 *  - the shared chat append ([addChat] → [SessionSync.dedupe] → `takeLast(2000)` → publish-if-
 *    current), late usage attach ([attachUsageToLastClaude]), cache wipe ([dropSessionCache]),
 *    and the view publish ([publish]);
 *  - the attached-gated status writes ([activityFor]/[usageFor]/[askFor]) — a background
 *    session's frame may only touch the live indicator/meter/ask popup when it is for the
 *    attached session (gate = [Host.attachedId]);
 *  - the filing bodies of the session-scoped frames themselves ([onSay], [onOutput],
 *    [onActivity], [onFiles], [onDiff], [onAsk], [onTranscript], [onErr], [onTurnInterrupted],
 *    [onTurnStopped]).
 *
 * What deliberately stays in each controller (behind [Host]) is the platform-specific side
 * effect: the StateFlow/settings wiring, the audio (speak/beep), and — crucially — the
 * **attach/detach/context-reset choreography** (writing the attach StateFlows + prefs) and the
 * **history-merge strategy**, both of which genuinely differ between the web (in-memory, index
 * sort) and Android (disk-backed, timestamp merge). Those talk to this router through [Host]
 * (or, for attach choreography, wrap a call to the router's log/keying primitives) rather than
 * moving their divergent logic in here — which is exactly what will let both share the router.
 *
 * [session] is the controller's own [SessionSync] instance (the digest/de-dup/turn-tracking
 * reconciler); this router files chat rows through it so the two stay in lockstep.
 */
class InboundRouter(
    private val session: SessionSync,
    private val host: Host,
) {
    /** The platform seam this router drives every side-effect through. */
    interface Host {
        /** Push the current view's chat rows into the UI's chat StateFlow. */
        fun publishChat(chat: List<ChatMessage>)
        /** Push whether the current view has an older history page to load. */
        fun setHasMore(hasMore: Boolean)
        /** Nudge the "scroll to bottom" tick after a new row lands in the current view. */
        fun bumpScroll()
        /** The attached session's stable id ("" when detached) — the attached-gate for
         *  [activityFor]/[usageFor]/[askFor]. */
        fun attachedId(): String
        /** Set the live "thinking/editing" breadcrumb for the attached session. */
        fun setActivity(text: String)
        /** Set the context-meter usage for the attached session (null clears it). */
        fun setUsage(usage: TurnUsageInfo?)
        /** Set (or clear) the pending-question popup for the attached session. */
        fun setAsk(questions: List<AskQuestion>?)
        /** Set the live hands-free draft text (cleared when a transcript commits). */
        fun setPending(text: String)
        /** Set the push-to-talk mic status line ("transcribing…" / ""). */
        fun setMicStatus(text: String)
        /** Surface a discover/session error string in the UI. */
        fun setDiscoverError(text: String)
        /** Clear the "usage report loading" spinner if it is currently showing. */
        fun stopUsageLoading()
        /** Locally bump a discovered session's recency/busy cue (mutates the discovered
         *  StateFlow, which stays controller-owned). */
        fun touchDiscovered(id: String, busy: Boolean?)
        /** Speak a reply aloud (server Kokoro or on-device TTS — the controller decides). */
        fun speak(text: String)
        /** Emit the summary-only "step happened" beep. */
        fun beep()
        /** Summary-only speech mode: beep through intermediate steps, speak only the result. */
        fun summaryOnly(): Boolean
        /** How many streamed replies of a turn to speak aloud before beeping the rest. */
        fun speakInitialReplies(): Int

        // --- Platform reactions with no web analogue -----------------------------
        // Android drives a lost-turn watchdog, a hands-free voice pill, pocket
        // notifications, and spoken notices; the web has none of these, so each is a
        // no-op default it simply doesn't override. Kept as explicit seams so the
        // drift-fixing step can give the web real behavior by filling the last four in
        // (speak the ask, speak an interruption, barge-in on stop) rather than
        // re-forking the filing logic.
        /** A turn is confirmed live (a chunk/activity arrived): mark it in-flight and
         *  disarm the lost-turn watchdog. */
        fun turnLive() {}
        /** A turn reached a terminal (close/say/ask/error/interrupt/stop): clear the
         *  in-flight flag and cancel the watchdog. */
        fun turnCleared() {}
        /** A turn's reply closed: surface it from the pocket when backgrounded. */
        fun turnDone(name: String, text: String) {}
        /** A committed transcript landed: chirp the "heard you" ack (when final) and
         *  move the voice pill to "thinking". */
        fun heardCommit(final: Boolean) {}
        /** Return the voice pill to "listening" if [id] is the attached session. */
        fun voiceIdleIfAttached(id: String) {}
        /** Speak the pending questions aloud if [id] is the attached session. */
        fun speakAskIfAttached(id: String, questions: List<AskQuestion>) {}
        /** React to a turn-interrupted terminal for [id] (voice pill + spoken notice)
         *  when it is the attached session. */
        fun turnInterruptedAttached(id: String) {}
        /** Barge-in: stop any reply being read aloud if [id] is the attached session. */
        fun bargeInIfAttached(id: String) {}
    }

    // --- Neutral per-session state (hoisted from the web controller) ----------

    // Per-session chat logs, keyed by the stable session id; [currentId] is the visible one
    // (a rename never re-keys these — the id is invariant). "" = the general/unattached view.
    val logs = mutableMapOf<String, List<ChatMessage>>()
    val oldest = mutableMapOf<String, Int>()
    val hasMore = mutableMapOf<String, Boolean>()
    var currentId = ""
    var loadingOlder = false
    val streamedSessions = mutableSetOf<String>()
    // Per-session count of streamed replies already spoken this turn, for the
    // "speak initial replies" refinement of summary-only mode. Reset each turn boundary.
    val spokenReplyCounts = mutableMapOf<String, Int>()

    /** The single message-keying rule: a frame's own session id, or the current view when the
     *  frame omits it (a hub-fanned frame / an older server). */
    fun keyOf(sessionId: String): String = sessionId.ifEmpty { currentId }

    // --- Shared filing primitives --------------------------------------------

    /** Push the current view's transcript + has-more flag into the UI StateFlows. */
    fun publish() {
        host.publishChat(logs[currentId] ?: emptyList())
        host.setHasMore(hasMore[currentId] ?: false)
    }

    /**
     * Append one chat row under [key] (default: the current view). Reconciles on the LIVE
     * path (see [SessionSync.dedupe]) — a hands-free utterance streams a live draft/echo row
     * and then lands the committed `transcript` as a second identical live row, which nothing
     * collapsed until a reattach; deduping here drops that adjacent duplicate as it's appended.
     * Publishes + bumps scroll when the row lands in the current view.
     */
    fun addChat(role: Role, text: String, usage: TokenUsage? = null, key: String = currentId) {
        if (text.isBlank()) return // never file an empty bubble (a blank say/output/transcript)
        val now = nowEpochSeconds()
        logs[key] = session.dedupe(
            (logs[key] ?: emptyList()) + ChatMessage(role, text, usage = usage, ts = now)
        ).takeLast(2000)
        if (key == currentId) {
            publish()
            host.bumpScroll()
        }
    }

    /** Late-attach a turn's usage badge to the last Claude row under [key] (the reply's live
     *  bubble already existed when its closing `output` arrived). */
    fun attachUsageToLastClaude(key: String, usage: TokenUsage) {
        val log = logs[key] ?: return
        val idx = log.indexOfLast { it.role == Role.CLAUDE }
        if (idx < 0) return
        logs[key] = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
        if (key == currentId) publish()
    }

    /**
     * Forget every trace of a session id's transcript (in-memory log + paging cursors +
     * held/server digests) so the next history fetch rebuilds it from scratch. Used when a
     * clear/compress retires a session_id server-side: the old rows carry stale indexes and
     * merging a fresh page over them would duplicate, so discard wholesale and refetch.
     */
    fun dropSessionCache(id: String) {
        logs.remove(id)
        hasMore.remove(id)
        oldest.remove(id)
        session.drop(id) // held + server digests
        if (id == currentId) publish()
    }

    // --- Attached-gated status writes ----------------------------------------
    // An incoming session-scoped frame may only touch these when it is for the attached
    // session, so a background turn can't flicker the indicator, pop a question, or clobber
    // the meter. Deliberate local transitions (attach/detach) still write the backing flows
    // directly in the controller; only frames off the wire route through these.

    fun activityFor(id: String, text: String) { if (id == host.attachedId()) host.setActivity(text) }
    fun usageFor(id: String, usage: TurnUsageInfo?) { if (id == host.attachedId()) host.setUsage(usage) }
    fun askFor(id: String, questions: List<AskQuestion>?) { if (id == host.attachedId()) host.setAsk(questions) }

    // --- Session-scoped frame filing bodies ----------------------------------

    fun onSay(msg: ServerMsg.Say) {
        // A `say` is also the terminal event for a background turn with no spoken Claude
        // reply (notably `compress`, which finishes with a confirmation say): end the turn.
        host.turnCleared()
        host.setMicStatus("") // a terminal `say` (e.g. "didn't catch that") ends the PTT clip; clear "transcribing…"
        // Hub-fanned says (compress done, breadcrumbs) carry their session id;
        // conversational dialog says don't → fall back to the current view.
        val key = keyOf(msg.sessionId)
        activityFor(key, "") // a background compress `say` must not clear the attached session's indicator
        // A turn-terminal say (compress done) can be redelivered buffered on
        // reconnect — its turn id drops the repeat. Breadcrumb says have no id.
        if (!session.terminalSeen(key, msg.turn)) {
            addChat(Role.SYSTEM, msg.text, key = key); host.speak(msg.text)
        }
    }

    fun onOutput(msg: ServerMsg.Output) {
        // Summary-only: beep through intermediate steps, speak only the final result.
        val summaryOnly = host.summaryOnly()
        val sid = keyOf(msg.sessionId) // stable id; the turn's session
        activityFor(sid, "") // prose/close is arriving — drop the indicator (attached session only)
        host.touchDiscovered(sid, msg.chunk) // reorder + working cue live
        if (msg.chunk) {
            // A streamed chunk proves the turn survived (like an activity breadcrumb) —
            // keep it in flight and disarm the interruption watchdog.
            host.turnLive()
            streamedSessions.add(sid)
            session.noteChunk(sid, msg.turn)
            addChat(Role.CLAUDE, msg.text, key = sid)
            if (sid == currentId) {
                if (summaryOnly) {
                    // Speak the first N replies of the turn aloud; beep the rest.
                    val spoken = spokenReplyCounts.getOrElse(sid) { 0 }
                    if (spoken < host.speakInitialReplies()) {
                        spokenReplyCounts[sid] = spoken + 1
                        host.speak(msg.text)
                        session.noteSpokenChunk(sid, msg.text, msg.turn)
                    } else {
                        host.beep()
                    }
                } else {
                    host.speak(msg.text)
                    session.noteSpokenChunk(sid, msg.text, msg.turn)
                }
            }
        } else {
            host.turnCleared() // the closing frame ends the turn
            // Same id-keyed close reconciliation as the Android controller:
            // redelivered = this close's turn was already decided on (buffered
            // resend / doubled close); streamed = its chunks reached us, by id
            // even when the legacy flag was cleared mid-turn. Query the ids
            // BEFORE shouldSpeakClose — that call records them.
            val redelivered = session.closeSeen(sid, msg.turn)
            val streamed = streamedSessions.remove(sid) ||
                session.closeStreamed(sid, msg.turn)
            spokenReplyCounts.remove(sid) // new turn restarts the initial-reply count
            val wantSpeak = session.shouldSpeakClose(sid, msg.text, summaryOnly, msg.turn)
            // A live bubble for this reply already exists when the turn streamed, but
            // also when a duplicate closing Output arrives for the same turn (backend
            // double-emit, or streamedSessions cleared mid-turn) — that second close
            // is what appended a second identical bubble. Reuse it in either case.
            val lastClaude = logs[sid]?.lastOrNull { it.role == Role.CLAUDE }
            val haveLiveBubble = lastClaude != null && lastClaude.index < 0 &&
                lastClaude.text.trim() == msg.text.trim()
            if (!streamed && !haveLiveBubble && !redelivered) {
                addChat(Role.CLAUDE, msg.text, msg.usage, key = sid)
            } else {
                if (msg.usage != null) attachUsageToLastClaude(sid, msg.usage)
            }
            if (wantSpeak && sid == currentId) host.speak(msg.text)
            // Anchor the cache-warm countdown to the turn's real completion
            // time (usage_at), not to when a buffered reply reached us.
            msg.usage?.let { u ->
                val ageMs = if (msg.usageAt > 0) (nowEpochSeconds() - msg.usageAt) * 1000 else 0L
                usageFor(sid, TurnUsageInfo(u, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))) // meter tracks the attached session only
            }
            host.turnDone(msg.name, msg.text) // surface it from the pocket when backgrounded
        }
    }

    fun onActivity(msg: ServerMsg.Activity) {
        // A live breadcrumb proves the turn is running server-side (it survived a
        // reconnect): mark it in flight and disarm the interruption watchdog.
        host.turnLive()
        val id = keyOf(msg.sessionId)
        activityFor(id, msg.text)
        host.touchDiscovered(id, true)
    }

    fun onFiles(msg: ServerMsg.Files) {
        if (msg.files.isNotEmpty()) {
            addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "), key = keyOf(msg.sessionId))
            if (host.summaryOnly()) host.beep() // intermediate step → beep like the rest
        }
    }

    fun onDiff(msg: ServerMsg.Diff) {
        addChat(Role.SYSTEM, "📊 diff:\n${msg.text}", key = keyOf(msg.sessionId))
        if (host.summaryOnly()) host.beep()
    }

    fun onAsk(msg: ServerMsg.Ask) {
        val key = keyOf(msg.sessionId)
        host.turnCleared() // an ask ends the turn in place of a closing output
        activityFor(key, ""); streamedSessions.remove(key); spokenReplyCounts.remove(key)
        host.touchDiscovered(key, false) // turn-terminal → clear the working cue
        host.voiceIdleIfAttached(key) // hands-free returns to listening (attached only)
        // An ask is a turn-terminal and can be redelivered buffered on reconnect
        // — keyed by its turn id; drop a repeat instead of re-presenting it.
        if (!session.terminalSeen(key, msg.turn)) {
            askFor(key, msg.questions) // only the attached session pops the question dialog
            addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q }, key = key)
            host.speakAskIfAttached(key, msg.questions) // …and only it is read aloud so you can answer by voice
        }
    }

    fun onTranscript(msg: ServerMsg.Transcript) {
        // Key by the session the clip was captured for (msg.sessionId), not the currently
        // attached one — otherwise switching sessions before the async transcript lands
        // files the user bubble under the wrong session. Older servers omit it → fall
        // back to the current view.
        val key = keyOf(msg.sessionId)
        askFor(key, null) // a spoken/typed reply answers the attached session's pending questions
        key.takeIf { it.isNotEmpty() }?.let { streamedSessions.remove(it); spokenReplyCounts.remove(it); session.noteTurnStart(it) }
        // The committed transcript supersedes the live hands-free draft — clear it
        // so the utterance isn't shown as both a draft and a committed bubble.
        host.setPending("")
        host.setMicStatus("") // the committed transcript ends the PTT clip; clear "transcribing…"
        addChat(Role.USER, msg.text, key = key)
        host.touchDiscovered(key, true) // dictation submitted → session is now working
        // Chirp the "heard you" ack (server recognized the utterance) and move the
        // voice pill to "thinking" before Claude replies.
        host.heardCommit(msg.final)
    }

    fun onErr(msg: ServerMsg.Err) {
        // Version skew: an older server rejects the connect-time `digest` probe
        // with bad_message — harmless (we fall back to fetching history), so
        // swallow it instead of a scary chat note (mirrors the Android client).
        if (msg.code == "bad_message" && msg.message.contains("digest")) return
        // A failed turn ends it: clear the streamed/spoken turn state so a later
        // stray close for the session isn't misread as "streamed" (Android parity).
        if (msg.code == "turn_failed") { host.turnCleared(); streamedSessions.clear(); spokenReplyCounts.clear() }
        host.setMicStatus("") // a transcribe_failed / not_implemented error ends the PTT clip; clear "transcribing…"
        host.stopUsageLoading()
        // Turn-terminal errors carry a session_id + turn id and can be redelivered
        // buffered on reconnect — drop the repeated row (state here is idempotent).
        val key = keyOf(msg.sessionId)
        activityFor(key, "") // only clear the indicator if the erroring turn is the attached session's
        if (session.terminalSeen(key, msg.turn)) return
        if (msg.code in setOf("session_active", "not_found", "bad_delete", "bad_adopt", "discover_failed")) {
            host.setDiscoverError(msg.message)
        } else addChat(Role.SYSTEM, "⚠️ ${msg.code}: ${msg.message}", key = key)
    }

    fun onTurnInterrupted(msg: ServerMsg.TurnInterrupted) {
        val key = keyOf(msg.sessionId)
        host.turnCleared()
        activityFor(key, ""); streamedSessions.remove(key); spokenReplyCounts.remove(key)
        host.turnInterruptedAttached(key) // voice pill + spoken "say it again" (attached only)
        addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.", key = key)
    }

    fun onTurnStopped(msg: ServerMsg.TurnStopped) {
        val key = keyOf(msg.sessionId)
        host.turnCleared()
        activityFor(key, ""); streamedSessions.remove(key); spokenReplyCounts.remove(key)
        host.voiceIdleIfAttached(key) // hands-free returns to listening (attached only)
        // A redelivered stop (buffered terminal, keyed by turn id) — drop the row.
        if (!session.terminalSeen(key, msg.turn)) {
            host.bargeInIfAttached(key) // a background stop must not cut off the reply you're hearing
            addChat(Role.SYSTEM, "⏹ stopped that turn.", key = key)
        }
    }
}

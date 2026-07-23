package com.bam.spawner

import android.content.Context
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageReport
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.audio.AudioRouter
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.audio.HandsFreeRecorder
import com.bam.spawner.audio.LevelMeter
import com.bam.spawner.audio.OpusRecorder
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ProfileInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SpawnerClient
import com.bam.spawner.tts.Markdown
import com.bam.spawner.tts.Speaker
import java.io.File
import java.util.concurrent.atomic.AtomicInteger
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

// --- Server-message handling, per-session chat/log paging, and history merge ---
// These are extension functions on VoiceController (identical to member functions;
// split out only to shrink the class file). Any state they touch is `internal` on
// VoiceController.

// migrateSessionKey re-keys every session-name-keyed piece of client state from
// old to new when a session is renamed, so nothing orphans under the stale name
// (an orphaned log empties the chat; an orphaned cursor breaks paging). This is
// the single site that must know the full set of name-keyed maps — a new one
// added above must be migrated here too.
internal fun VoiceController.migrateSessionKey(old: String, new: String) {
    logs.remove(old)?.let { logs[new] = it }
    oldestIndex.remove(old)?.let { oldestIndex[new] = it }
    hasMore.remove(old)?.let { hasMore[new] = it }
    if (loadingOlder.remove(old)) loadingOlder.add(new)
    bridgeTo.remove(old)?.let { bridgeTo[new] = it }
    session.migrate(old, new) // held + server digests
    if (loadedFromCache.remove(old)) loadedFromCache.add(new)
    cache.remove(old) // drop the stale file; the new key is repersisted on the next persist()
}

// dropSessionCache forgets every cached/paged trace of a session's transcript
// (all name-keyed maps + the on-disk file) so the next history fetch rebuilds it
// from scratch. Used when a clear/compress rotates the session_id server-side: the
// old rows are stale (the conversation was wiped/summarized), and merging a small
// fresh page over rows carrying the old indexes would leave duplicates — so we
// discard wholesale and refetch instead. (This must know the same name-keyed set
// as migrateSessionKey; keep the two in sync when a new keyed map is added.)
internal fun VoiceController.dropSessionCache(name: String) {
    logs.remove(name)
    oldestIndex.remove(name)
    hasMore.remove(name)
    loadingOlder.remove(name)
    bridgeTo.remove(name)
    session.drop(name) // held + server digests
    loadedFromCache.remove(name)
    cache.remove(name)
    if (name == currentKey) _chat.value = emptyList()
}

// ensureLoaded pulls a session's persisted transcript from disk into the
// in-memory maps the first time it's needed (so the cached chat shows even
// offline), without clobbering a live in-memory log we already hold.
internal fun VoiceController.ensureLoaded(name: String) {
    if (name.isEmpty() || name in loadedFromCache) return
    loadedFromCache.add(name)
    if (name in logs) return
    val c = cache.load(name) ?: return
    logs[name] = session.dedupe(c.messages.map { it.toChat() })
    oldestIndex[name] = c.oldestIndex
    hasMore[name] = c.hasMore
    session.recordHeld(name, c.count, c.hash)
}

// persist writes a session's current log (minus live-only SYSTEM notes, which
// aren't part of the server transcript) plus its paging cursor and held digest
// to disk, so it survives an app restart and can be shown offline.
internal fun VoiceController.persist(name: String) {
    if (name.isEmpty()) return
    val msgs = session.dedupe(logs[name] ?: return)
    logs[name] = msgs
    val keep = msgs.filter { it.role != Role.SYSTEM }
    val d = session.heldDigest(name)
    cache.save(name, CachedSession(
        messages = keep.map { it.toCached() },
        oldestIndex = oldestIndex[name] ?: (keep.firstOrNull { it.index >= 0 }?.index ?: 0),
        hasMore = hasMore[name] ?: false,
        count = d?.first ?: 0,
        hash = d?.second ?: "",
    ))
}

internal fun VoiceController.focusKnownSession(target: DiscoveredInfo, syncServer: Boolean) {
    if (target.sessionId.isBlank()) {
        client?.send(Outbound.attach(target.name, silent = syncServer))
        return
    }
    session.rememberPreviousIfSwitching(target.sessionId)
    clearTurnInFlight()
    _activity.value = ""
    _pending.value = ""
    _lastTurnUsage.value = null
    _attachedId.value = target.sessionId
    _attachedName.value = target.name
    _attachedAgent.value = target.agent
    _attachedModel.value = target.model
    settings.lastSession = target.name
    settings.lastSessionId = target.sessionId
    _status.value = "attached: ${target.name}"
    showLog(target.name)
    session.requestFreshHistory(target.name)
    if (syncServer) client?.send(Outbound.attach(target.name, sessionId = target.sessionId, silent = true))
}

/** Locally bump a session's sidebar metadata (recency + busy cue) the instant a
 *  message arrives, so the list re-sorts and shows "working…" without waiting for
 *  the next `discover` round trip. A no-op if the session isn't in the list yet
 *  (a later discover fills it in). Not written to the disk cache — the authoritative
 *  snapshot still comes from the server's `discovered` frame. */
internal fun VoiceController.touchDiscovered(name: String, busy: Boolean? = null) {
    if (name.isEmpty()) return
    val now = System.currentTimeMillis() / 1000
    var changed = false
    val next = _discovered.value.map { d ->
        if (d.name == name) {
            changed = true
            d.copy(lastActive = maxOf(d.lastActive, now), busy = busy ?: d.busy)
        } else d
    }
    if (changed) _discovered.value = next
}

/** Request the page of history just older than what we hold for `name`. Shared by
 *  the user's scroll-back (loadOlder) and the reconnect gap-fill in onHistory. */
internal fun VoiceController.fetchOlder(name: String) {
    if (name.isEmpty() || hasMore[name] != true || name in loadingOlder) return
    val before = oldestIndex[name] ?: return
    loadingOlder.add(name)
    client?.send(Outbound.history(name, before))
}

internal fun VoiceController.onMessage(msg: ServerMsg) {
    // Exhaustive over the ServerMsg sealed interface — deliberately NO `else`
    // branch, so adding a new server message fails to compile until it's handled
    // here. Unknown (an unrecognized wire type) is its own explicit no-op case;
    // don't collapse it into an `else` or that compile-time guard is lost.
    when (msg) {
        is ServerMsg.HelloOk -> onHelloOk(msg)
        is ServerMsg.WhisperModel -> onWhisperModel(msg)
        is ServerMsg.WhisperDownload -> onWhisperDownload(msg)
        is ServerMsg.Say -> onSay(msg)
        is ServerMsg.Output -> onOutput(msg)
        is ServerMsg.ContextReset -> onContextReset(msg)
        is ServerMsg.Activity -> onActivity(msg)
        is ServerMsg.Transcribing -> onTranscribing(msg)
        is ServerMsg.Files -> onFiles(msg)
        is ServerMsg.Diff -> onDiff(msg)
        is ServerMsg.RateLimit -> _rateLimit.value = msg.info // plan session-limit readout (sidebar)
        is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report } // opens the usage sheet
        is ServerMsg.Ask -> onAsk(msg)
        is ServerMsg.Transcript -> onTranscript(msg)
        is ServerMsg.Pending -> onPending(msg)
        is ServerMsg.Calibration -> onCalibrationSample(msg.text)
        is ServerMsg.StopSpeaking -> {
            cancelServerSpeech()
            speaker.stop()
        }
        is ServerMsg.SpeakAudio -> onSpeakAudio(msg)
        is ServerMsg.SpeakEnd -> onSpeakEnd(msg)
        is ServerMsg.TtsVoices -> onTtsVoices(msg)
        is ServerMsg.SpeechMode -> settings.summaryOnlySpeech = msg.summaryOnly // "summary only" / "speak everything" voice toggle
        is ServerMsg.Dialog -> _status.value = "dialog: ${msg.state}"
        is ServerMsg.Attached -> onAttached(msg)
        is ServerMsg.Detached -> onDetached(msg)
        is ServerMsg.Renamed -> onRenamed(msg)
        is ServerMsg.History -> onHistory(msg)
        is ServerMsg.ReadLast -> onReadLast(msg.count)
        is ServerMsg.Discovered -> onDiscovered(msg)
        is ServerMsg.Listing -> _listing.value = msg
        is ServerMsg.FileSaved -> _fileSaved.tryEmit(msg.path)
        is ServerMsg.FileData -> _fileData.tryEmit(msg)
        is ServerMsg.Digests -> {
            // Connect-time server-truth sweep. No longer consulted: transcript freshness
            // is checked per-attach via `have_hash` → `unchanged` (see requestFreshHistory),
            // which — unlike a cached connect snapshot — can't go stale for a session we're
            // detached from and so silently drop its messages. Kept as a protocol no-op.
        }
        is ServerMsg.HostList, is ServerMsg.IdentityList,
        is ServerMsg.Agents, is ServerMsg.Profiles,
        is ServerMsg.SpokenTokens -> catalogues.apply(msg)
        is ServerMsg.Actions -> _spokenActions.value = msg.actions
        is ServerMsg.Settings -> { catalogues.apply(msg); mirrorSettingsToPrefs() }
        is ServerMsg.Err -> onErr(msg)
        is ServerMsg.TurnInterrupted -> onTurnInterrupted(msg)
        is ServerMsg.TurnStopped -> onTurnStopped(msg)
        is ServerMsg.Unknown -> {}
    }
}

internal fun VoiceController.onHelloOk(msg: ServerMsg.HelloOk) {
    _status.value = "connected"
    if (msg.whisperModel.isNotBlank()) { // adopt the server's current model
        _whisperModel.value = msg.whisperModel
        settings.whisperModel = msg.whisperModel
    }
    // Unconditional: "" is meaningful (no fast server configured there).
    _whisperFastModel.value = msg.whisperModelFast
    settings.whisperFastModel = msg.whisperModelFast
    _whisperModels.value = msg.whisperModels
    _whisperModelsLocal.value = msg.whisperModelsLocal
    _serverTtsAvailable.value = msg.tts
    if (msg.tts) client?.send(Outbound.ttsVoices()) // fetch the voice-picker catalogue
    discover() // the drawer lists ALL machine sessions (discovery is the source)
    client?.send(Outbound.digest()) // validate the offline transcript cache (bodies-free)
    settings.lastSession.takeIf { it.isNotEmpty() }?.let {
        // Prefer the stable id so we re-attach to the SAME session even when it's
        // named differently on this server (e.g. after switching servers).
        client?.send(Outbound.attach(it, sessionId = settings.lastSessionId, silent = true))
    }
}

internal fun VoiceController.onWhisperModel(msg: ServerMsg.WhisperModel) {
    if (msg.model.isNotBlank()) { _whisperModel.value = msg.model; settings.whisperModel = msg.model }
    _whisperFastModel.value = msg.fastModel
    settings.whisperFastModel = msg.fastModel
    if (msg.models.isNotEmpty()) _whisperModels.value = msg.models
    _whisperModelsLocal.value = msg.local
}

internal fun VoiceController.onWhisperDownload(msg: ServerMsg.WhisperDownload) {
    // Clear the banner once a download completes cleanly; keep it on error so
    // the failure is visible, and while in flight to drive the progress bar.
    _whisperDownload.value =
        if (msg.done && msg.error.isBlank()) null
        else WhisperDownloadInfo(msg.model, msg.fast, msg.received, msg.total, msg.done, msg.error)
}

internal fun VoiceController.onSay(msg: ServerMsg.Say) {
    // A `say` is also the terminal event for a background turn that has no
    // spoken Claude reply — notably `compress`, which finishes with a
    // confirmation `say` rather than an `output`. Clear the in-flight/activity
    // state so the "…compressing… ⏹ stop" bar dismisses; otherwise it lingers
    // and tapping stop aborts an already-finished turn ("nothing running to stop").
    clearTurnInFlight()
    _activity.value = ""
    _mic.value = "" // a terminal `say` (e.g. "didn't catch that") ends the PTT clip; clear "transcribing…"
    // A turn-terminal say (compress done) can be redelivered buffered on
    // reconnect — its turn id drops the repeat. Breadcrumb says have no id.
    if (!session.terminalSeen(currentKey, msg.turn)) {
        addChat(Role.SYSTEM, msg.text); speakText(Markdown.toSpeech(msg.text))
    }
}

internal fun VoiceController.onOutput(msg: ServerMsg.Output) {
    // Summary-only mode: don't read the intermediate streamed steps aloud —
    // play a soft beep as a "still working…" cue and speak only the final
    // result when the turn closes. Everything is still shown in the chat.
    val summaryOnly = settings.summaryOnlySpeech
    touchDiscovered(msg.name, busy = msg.chunk) // reorder + working cue live
    if (msg.chunk) {
        // A live segment of Claude's reply as it's produced. Show it now; a
        // streamed chunk also proves the turn survived (like activity), so
        // keep it in flight and disarm the interruption watchdog.
        turnInFlight = true
        lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
        streamedSessions.add(msg.name)
        session.noteChunk(msg.name, msg.turn)
        _activity.value = "" // prose is arriving — drop the "thinking" breadcrumb
        addChat(Role.CLAUDE, msg.text, key = msg.name)
        if (msg.name == currentKey) {
            if (summaryOnly) {
                // Speak the first N replies of the turn aloud; beep the rest.
                val spoken = spokenReplyCounts.getOrElse(msg.name) { 0 }
                if (spoken < settings.speakInitialReplies) {
                    spokenReplyCounts[msg.name] = spoken + 1
                    speakText(Markdown.toSpeech(msg.text))
                    session.noteSpokenChunk(msg.name, msg.text, msg.turn)
                } else {
                    speaker.beep()
                }
            } else {
                speakText(Markdown.toSpeech(msg.text))
                session.noteSpokenChunk(msg.name, msg.text, msg.turn)
            }
        }
    } else {
        clearTurnInFlight()
        _activity.value = "" // turn done — stop the thinking indicator
        // The close's `turn` id is the authoritative link to its chunks (query
        // the reconciler BEFORE shouldSpeakClose — that call records the id):
        // redelivered = a close for a turn already closed (buffered-final
        // resend / doubled close) — never a new bubble; streamed = this turn's
        // chunks reached us, by id or (pre-turn-id server) the legacy flag.
        val redelivered = session.closeSeen(msg.name, msg.turn)
        val streamed = streamedSessions.remove(msg.name) ||
            session.closeStreamed(msg.name, msg.turn)
        spokenReplyCounts.remove(msg.name) // new turn starts the initial-reply count over
        // Does a live bubble for this exact reply already exist? It does when the
        // turn streamed (built from chunks), but ALSO when a duplicate closing
        // Output arrives for the same turn after streamedSessions was cleared
        // mid-turn (an interleaved Ask/Transcript/error) or the backend emits the
        // final message twice — that second close is what appended a second
        // identical bubble. Reuse the existing bubble in either case.
        // Whether to VOICE this close is decided by the shared reconciler
        // (SessionSync), keyed on the turn id when the server sends one (text
        // equality between chunks and close is not guaranteed) and falling back
        // to the voiced-text comparison when it doesn't. Must be called exactly
        // once per closing frame.
        val wantSpeak = session.shouldSpeakClose(msg.name, msg.text, summaryOnly, msg.turn)
        val lastClaude = logs[msg.name]?.lastOrNull { it.role == Role.CLAUDE }
        val haveLiveBubble = lastClaude != null && lastClaude.index < 0 &&
            lastClaude.text.trim() == msg.text.trim()
        if (!streamed && !haveLiveBubble && !redelivered) { // genuinely no live stream reached us (buffered reply on reconnect)
            addChat(Role.CLAUDE, msg.text, msg.usage, key = msg.name)
        } else {
            // Streamed (or already-shown) turn: the bubble exists, so badge it in
            // place — the closing message isn't re-rendered as a new bubble.
            if (msg.usage != null) attachUsageToLastClaude(msg.name, msg.usage)
        }
        if (wantSpeak && msg.name == currentKey) speakText(Markdown.toSpeech(msg.text))
        // Anchor the cache-warm countdown to the turn's real completion
        // time (usage_at), so a reply delivered buffered on reconnect
        // counts down from its true age, not from when it arrived.
        msg.usage?.let { u ->
            val ageMs = if (msg.usageAt > 0) System.currentTimeMillis() - msg.usageAt * 1000 else 0L
            _lastTurnUsage.value = TurnUsageInfo(u, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))
        }
        if (!appForeground) notifier.turnDone(msg.name, msg.text) // surface it from the pocket
    }
}

internal fun VoiceController.onContextReset(msg: ServerMsg.ContextReset) {
    _lastTurnUsage.value = null // context cleared → status bar returns to 0
    // A clear/compress rotates the session_id server-side and wipes/
    // summarizes the transcript. The rotated id now rides only on this
    // message (the server no longer re-emits `attached`), so treat it as a
    // rotation: re-key the attached id, drop the now-stale cached rows for
    // this session, and refetch fresh history. An old server omits
    // session_id — then this is a meter reset only (preserve old behavior).
    if (msg.sessionId.isNotEmpty()) {
        if (_attachedName.value == msg.name) {
            _attachedId.value = msg.sessionId
            settings.lastSessionId = msg.sessionId
        }
        dropSessionCache(msg.name)
        session.requestFreshHistory(msg.name)
    }
}

internal fun VoiceController.onActivity(msg: ServerMsg.Activity) {
    // A live breadcrumb means the turn is running server-side; mark it in
    // flight and disarm any interruption watchdog (it survived a reconnect).
    turnInFlight = true
    lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
    _activity.value = msg.text
    touchDiscovered(currentKey, busy = true)
}

internal fun VoiceController.onTranscribing(msg: ServerMsg.Transcribing) {
    // A committed hands-free clip is being re-transcribed accurately.
    // Show "transcribing…" instead of flashing back to "listening" until
    // the transcript lands (which flips this to "thinking…").
    if (hfOn) _voiceState.value = VoiceState.TRANSCRIBING
}

internal fun VoiceController.onFiles(msg: ServerMsg.Files) {
    if (msg.files.isNotEmpty()) {
        // A changed-files note hits the chat like any intermediate step, so in
        // summary-only mode it beeps too — otherwise these slip by silently.
        addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "))
        if (settings.summaryOnlySpeech) speaker.beep()
    }
}

internal fun VoiceController.onDiff(msg: ServerMsg.Diff) {
    addChat(Role.SYSTEM, "📊 diff:\n${msg.text}") // review summary, not spoken
    if (settings.summaryOnlySpeech) speaker.beep()
}

internal fun VoiceController.onAsk(msg: ServerMsg.Ask) {
    clearTurnInFlight()
    streamedSessions.remove(msg.name)
    spokenReplyCounts.remove(msg.name)
    _activity.value = ""
    touchDiscovered(msg.name, busy = false) // turn-terminal → clear the working cue
    if (hfOn) _voiceState.value = VoiceState.LISTENING
    // An ask is a turn-terminal (it ends the turn in place of the closing
    // output) and can be redelivered buffered on reconnect — keyed by its
    // turn id. Re-presenting is harmless for the chat row (dedupe folds it)
    // but re-SPEAKING the questions is not; drop a seen terminal outright.
    if (!session.terminalSeen(msg.name, msg.turn)) {
        _ask.value = msg.questions
        addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q }, key = msg.name)
        speakText(spokenQuestions(msg.questions)) // read aloud so you can answer by voice
    }
}

internal fun VoiceController.onTranscript(msg: ServerMsg.Transcript) {
    _ask.value = null // a spoken/typed reply answers any pending questions
    (_attachedName.value ?: currentKey).takeIf { it.isNotEmpty() }?.let { streamedSessions.remove(it); spokenReplyCounts.remove(it); session.noteTurnStart(it) }
    // The committed transcript supersedes the live hands-free draft — drop the
    // greyed draft line so the utterance isn't shown as both a draft and a bubble.
    _pending.value = ""
    addChat(Role.USER, msg.text); _mic.value = ""
    touchDiscovered(_attachedName.value ?: currentKey, busy = true) // dictation submitted → session is now working
    // Chirp the "heard you" acknowledgment: the server has recognized the
    // utterance and is dispatching it to the session, so confirm receipt
    // now — before Claude replies (and distinct from its activity beep).
    if (msg.final) speaker.chirp()
    if (hfOn) _voiceState.value = VoiceState.THINKING
}

internal fun VoiceController.onPending(msg: ServerMsg.Pending) {
    _pending.value = msg.text
    if (msg.text.isEmpty()) cancelSilenceCommit() // committed/cleared
    if (hfOn) _voiceState.value = if (msg.text.isEmpty()) VoiceState.LISTENING else VoiceState.CAPTURING
}

internal fun VoiceController.onTtsVoices(msg: ServerMsg.TtsVoices) {
    if (msg.error.isEmpty()) {
        _ttsVoices.value = msg.voices
        _ttsVoiceDefault.value = msg.defaultVoice
    }
}

internal fun VoiceController.onAttached(msg: ServerMsg.Attached) {
    session.rememberPreviousOnAttach(msg.name, msg.sessionId)
    // A backend switch (set_agent) rotates the session_id but keeps the name and
    // re-emits `attached` (not context_reset). If this is that rotation of the
    // session we're already on — same name, different id — the rows we hold are
    // the wiped old backend's, so drop them (and the digests) before requesting
    // history below, exactly like context_reset. Reads the still-held id/name, so
    // it must run before we overwrite them. A same-id re-attach drops nothing.
    session.onAttachRotation(msg.name, msg.sessionId)
    // Fresh view of this session: drop any stale turn spinner/watchdog.
    // If a turn is genuinely still running, the server's bindJob sends a
    // "still working" breadcrumb right after this (which re-arms it); if
    // the turn finished while we were away, nothing comes and the spinner
    // correctly stays clear instead of hanging on "running the command".
    clearTurnInFlight()
    _activity.value = ""
    // Seed the context meter from the transcript's last turn so the size
    // (and how much a clear/compress would reclaim) shows immediately,
    // before any live turn. Anchor the cache-warm countdown to that turn's
    // real age so it reads warm only if it genuinely still is; no usage
    // (fresh session) leaves the meter blank.
    _lastTurnUsage.value = msg.usage?.let { u ->
        val ageMs = if (msg.usageAt > 0) System.currentTimeMillis() - msg.usageAt * 1000 else Long.MAX_VALUE
        TurnUsageInfo(u, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))
    }
    _attachedId.value = msg.sessionId
    _attachedName.value = msg.name
    _attachedAgent.value = msg.agent
    _attachedModel.value = msg.model
    settings.lastSession = msg.name
    settings.lastSessionId = msg.sessionId
    _status.value = "attached: ${msg.name}"
    showLog(msg.name)
    // Refetch recent history on (re)attach so a session that produced output
    // while we viewed another one isn't left stale (the server only fans live
    // output to the currently-attached connection). But save data when we can:
    // if the connect-time digest sweep says this session's server hash still
    // equals what our cache holds — and we actually have cached content — the
    // transcript is unchanged, so skip the fetch entirely. Otherwise ask for
    // the recent page, passing the hash we hold so the server can still answer
    // `unchanged` (no bodies) if nothing moved. onHistory dedupes against live.
    session.requestFreshHistory(msg.name)
}

internal fun VoiceController.onDetached(msg: ServerMsg.Detached) {
    session.rememberPrevious()
    _attachedId.value = ""
    _attachedName.value = null
    _attachedAgent.value = ""
    _attachedModel.value = ""
    settings.lastSession = ""
    settings.lastSessionId = ""
    _status.value = "connected"
    showLog("")
}

internal fun VoiceController.onRenamed(msg: ServerMsg.Renamed) {
    // Follow a rename of the session we're attached to so the title bar
    // tracks the sidebar. Match by the stable session id (the title's name
    // may be stale — e.g. a leftover from another server — so a name compare
    // misses); fall back to the old name only when the server sent no id.
    // In-place update only — no history refetch or meter reseed (unlike a
    // full re-attach). Client state is keyed by name, so migrate every keyed
    // map or the chat/paging orphans.
    val mine = if (msg.sessionId.isNotEmpty()) _attachedId.value == msg.sessionId
    else _attachedName.value == msg.old
    if (mine) {
        val from = _attachedName.value ?: msg.old
        migrateSessionKey(from, msg.name)
        if (currentKey == from) currentKey = msg.name
        _attachedName.value = msg.name
        settings.lastSession = msg.name
        _status.value = "attached: ${msg.name}"
    }
}

internal fun VoiceController.onDiscovered(msg: ServerMsg.Discovered) {
    _discovered.value = msg.sessions
    discoveredCache.save(msg.sessions)
    _discoverError.value = ""
    // Re-derive the attached title from the fresh list by stable id. After a
    // server switch the same session can carry a different name here, leaving
    // the title stale; if the current server calls our attached id something
    // else, migrate the title (and name-keyed state) to match it.
    if (_attachedId.value.isNotEmpty()) {
        val cur = msg.sessions.find { it.sessionId == _attachedId.value }?.name
        if (cur != null && cur != _attachedName.value) {
            _attachedName.value?.let { from ->
                migrateSessionKey(from, cur)
                if (currentKey == from) currentKey = cur
            }
            _attachedName.value = cur
            settings.lastSession = cur
            _status.value = "attached: $cur"
        }
    }
}

internal fun VoiceController.onErr(msg: ServerMsg.Err) {
    // Version skew: an older server that predates the transcript-cache feature
    // rejects our connect-time `digest` probe with bad_message. That's harmless
    // (we just get no digests and fall back to fetching history), so swallow it
    // instead of spamming a scary note in the chat during a rollout.
    if (msg.code == "bad_message" && msg.message.contains("digest")) return
    if (msg.code == "turn_failed") { clearTurnInFlight(); streamedSessions.clear(); spokenReplyCounts.clear() }
    if (_usageLoading.value) _usageLoading.value = false // any error unsticks a pending usage fetch
    _activity.value = ""
    _mic.value = "" // a transcribe_failed / not_implemented error ends the PTT clip; clear "transcribing…"
    // Turn-terminal errors (turn_failed / compress failures) carry a turn id
    // and can be redelivered buffered on reconnect — drop the repeated row.
    // The state clearing above is idempotent and safe to re-run either way.
    if (session.terminalSeen(currentKey, msg.turn)) return
    // Discover/adopt/delete errors surface on the Discover screen; the
    // rest go to the chat log.
    if (msg.code in setOf("session_active", "not_found", "bad_adopt", "bad_delete", "discover_failed")) {
        _discoverError.value = msg.message
    } else {
        addChat(Role.SYSTEM, "⚠️ ${msg.code}: ${msg.message}")
    }
}

internal fun VoiceController.onTurnInterrupted(msg: ServerMsg.TurnInterrupted) {
    clearTurnInFlight()
    streamedSessions.remove(msg.name)
    spokenReplyCounts.remove(msg.name)
    _activity.value = ""
    if (hfOn) _voiceState.value = VoiceState.LISTENING
    addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.", key = msg.name)
    speakText("that turn got interrupted — the server restarted. say it again.")
}

internal fun VoiceController.onTurnStopped(msg: ServerMsg.TurnStopped) {
    clearTurnInFlight()
    streamedSessions.remove(msg.name)
    spokenReplyCounts.remove(msg.name)
    _activity.value = ""
    if (hfOn) _voiceState.value = VoiceState.LISTENING
    // A redelivered stop (buffered terminal, keyed by turn id) must not
    // silence whatever is being read NOW or re-add its row.
    if (!session.terminalSeen(msg.name, msg.turn)) {
        cancelServerSpeech()
        speaker.stop() // also quiet any reply already being read
        addChat(Role.SYSTEM, "⏹ stopped that turn.", key = msg.name)
    }
}

// addChat appends a live message to the named session's log and reflects it
// only when that session is the visible view. Historical messages come via
// onHistory instead.
internal fun VoiceController.addChat(role: Role, text: String, usage: TokenUsage? = null, key: String = currentKey) {
    if (text.isBlank()) return
    // Run the shared reconciler on the LIVE path, not only on a history merge: a
    // hands-free utterance streams a live draft/echo row and then lands the committed
    // `transcript` as a second identical live row, and both are index==-1 — so nothing
    // collapsed them until a full reattach. Deduping here drops that adjacent duplicate
    // the moment it's appended (see SessionSync.dedupe).
    val updated = session.dedupe(
        (logs[key] ?: emptyList()) + ChatMessage(role, text, usage = usage, ts = System.currentTimeMillis() / 1000)
    ).takeLast(2000)
    logs[key] = updated
    if (key == currentKey) _chat.value = updated
}

// attachUsageToLastClaude badges the most recent Claude bubble in the named
// log with a completed turn's token usage. Used when the reply streamed live
// (the bubble was built from chunks, so the closing message can't add a new one).
internal fun VoiceController.attachUsageToLastClaude(key: String, usage: TokenUsage) {
    val log = logs[key] ?: return
    val idx = log.indexOfLast { it.role == Role.CLAUDE }
    if (idx < 0) return
    val updated = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
    logs[key] = updated
    if (key == currentKey) _chat.value = updated
}

/** Switch the visible chat to `key`'s log (session name, or "" for general). */
internal fun VoiceController.showLog(key: String) {
    if (currentKey != key) persist(currentKey) // save what we were viewing (captures live-streamed tail)
    currentKey = key
    ensureLoaded(key) // fault the cached transcript in from disk if we don't have it live
    _chat.value = logs[key] ?: emptyList()
    _hasMoreHistory.value = hasMore[key] ?: false
    scrollToBottom() // attaching / switching → show the latest (history refresh re-scrolls)
}

// ordered returns the log sorted chronologically by timestamp. History carries
// server transcript time; live messages carry the phone's wall clock at arrival
// (addChat) — both unix seconds, so they interleave correctly. Two guards keep it
// safe: (1) messages predating transcript timestamps have ts==0, so a timestamp is
// carried forward from the preceding message (computed on the pre-sort order, where
// the zeros sit contiguously at the front of the history block) instead of letting
// them float to the top; (2) the sort is stable, so equal timestamps preserve the
// existing order (history ahead of the live tail, live in arrival order).
internal fun VoiceController.ordered(msgs: List<ChatMessage>): List<ChatMessage> {
    var carried = 0L
    val stamped = msgs.map { m ->
        if (m.ts > 0L) carried = m.ts
        m to carried
    }
    return stamped.sortedBy { it.second }.map { it.first }
}

// onHistory merges a server-served page of OLDER messages into the session's
// log, ordered chronologically with any live messages, and updates the paging cursor.
internal fun VoiceController.onHistory(msg: ServerMsg.History) {
    // `unchanged` answers a top-page request whose have_hash still matched: our
    // cached transcript is current, so keep it untouched and just refresh the
    // stored digest (both held and server) so future freshness checks stand.
    if (msg.unchanged) {
        if (msg.hash.isNotEmpty()) session.recordSynced(msg.name, msg.count, msg.hash)
        loadingOlder.remove(msg.name)
        logs[msg.name]?.let { cleaned ->
            val deduped = session.dedupe(cleaned)
            logs[msg.name] = deduped
            if (msg.name == currentKey) _chat.value = deduped
        }
        persist(msg.name)
        return
    }
    val wasLoadOlder = msg.name in loadingOlder // else it's the top page (on (re)attach)
    // Highest transcript index we already held before applying this page — the
    // watermark a reconnect must page back down to so no middle stays missing.
    val heldMax = (logs[msg.name] ?: emptyList()).mapNotNull { it.index.takeIf { i -> i >= 0 } }.maxOrNull()
    val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts) }
    val histIdx = hist.mapNotNull { if (it.index >= 0) it.index else null }.toSet()
    // On a top reload (an attach/reattach), the history page is the authoritative
    // tail of the conversation: drop any live (index < 0) copy whose text now
    // appears in it, so refetching on reattach doesn't duplicate a reply we'd
    // already streamed. Live messages absent from the page (a turn still streaming,
    // not yet persisted) are kept. A load-older page leaves live messages untouched.
    val histTexts = if (wasLoadOlder) emptySet() else hist.map { it.role to it.text }.toSet()
    val existing = (logs[msg.name] ?: emptyList()).filter {
        (it.index < 0 && (it.role to it.text) !in histTexts) || (it.index >= 0 && it.index !in histIdx)
    }
    // Merge by timestamp, not by concatenation: a surviving live message (e.g. a
    // mid-turn breadcrumb not present in the fetched page) may be OLDER than the
    // history block, so `hist + existing` would strand it at the bottom, out of
    // order. Ordering by ts drops it back into its true chronological slot.
    logs[msg.name] = session.dedupe(ordered(hist + existing))
    if (msg.messages.isNotEmpty()) oldestIndex[msg.name] = msg.messages.first().index
    hasMore[msg.name] = msg.more
    loadingOlder.remove(msg.name)
    // Record the chain digest this page belongs to and persist the merged log, so
    // the cache is current on disk and a later reattach can short-circuit the fetch.
    if (msg.hash.isNotEmpty()) session.recordSynced(msg.name, msg.count, msg.hash)
    persist(msg.name)
    // Reconnect gap-fill: the reattach top page is only the newest slice, so if the
    // session advanced by more than a page while we were away, a hole is left between
    // what we still held (heldMax) and this page's oldest index. Mark the watermark,
    // then keep paging older until we reconnect with it (or hit the start) so the
    // whole gap backfills instead of only the newest page.
    if (!wasLoadOlder && heldMax != null) {
        val pageOldest = msg.messages.firstOrNull()?.index
        if (pageOldest != null && pageOldest > heldMax + 1) bridgeTo[msg.name] = heldMax
    }
    bridgeTo[msg.name]?.let { target ->
        val oldest = oldestIndex[msg.name]
        if (oldest != null && oldest > target + 1 && hasMore[msg.name] == true) {
            fetchOlder(msg.name) // still a hole above the watermark — keep paging
        } else {
            bridgeTo.remove(msg.name) // reconnected with what we had (or reached the start)
        }
    }
    if (msg.name == currentKey) {
        _chat.value = logs[msg.name] ?: emptyList()
        _hasMoreHistory.value = msg.more
        if (!wasLoadOlder) scrollToBottom() // initial load → newest in view; load-older stays put
    }
}

// onReadLast re-reads (TTS) and scrolls to the last `count` Claude replies in
// the current view.
internal fun VoiceController.onReadLast(count: Int) {
    val claude = _chat.value.filter { it.role == Role.CLAUDE }.takeLast(count.coerceAtLeast(1))
    if (claude.isEmpty()) {
        speakText("nothing to read yet")
    } else {
        speakText(claude.joinToString(". … ") { Markdown.toSpeech(it.text) })
    }
    _scrollTick.value = _scrollTick.value + 1
}

internal fun VoiceController.roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE

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
import kotlinx.coroutines.withContext

// --- Server-message handling, per-session chat/log paging, and history merge ---
// These are extension functions on VoiceController (identical to member functions;
// split out only to shrink the class file). Any state they touch is `internal` on
// VoiceController.

// dropSessionCache forgets every cached/paged trace of a session id's transcript
// (all id-keyed maps + the on-disk file) so the next history fetch rebuilds it from
// scratch. Used when a clear/compress retires a session_id server-side: the old rows
// are stale (the conversation was wiped/summarized), and merging a small fresh page
// over rows carrying the old indexes would leave duplicates — so we discard wholesale
// and refetch instead.
internal fun VoiceController.dropSessionCache(id: String) {
    router.logs.remove(id)
    router.oldest.remove(id)
    router.hasMore.remove(id)
    loadingOlder.remove(id)
    bridgeTo.remove(id)
    session.drop(id) // held + server digests
    loadedFromCache.remove(id)
    cache.remove(id)
    if (id == router.currentId) _chat.value = emptyList()
}

// ensureLoaded pulls a session's persisted transcript from disk into the
// in-memory maps the first time it's needed (so the cached chat shows even
// offline), without clobbering a live in-memory log we already hold. Keyed by
// the stable session id (an older name-keyed cache file simply misses → rebuilt
// from history on the next fetch). A memory hit applies inline (so the switch renders the cached chat in the same
// frame); a miss reads the file on a background dispatcher and applies + republishes
// when it lands, so the click lambda never waits on disk.
internal fun VoiceController.ensureLoaded(id: String) {
    if (id.isEmpty() || id in loadedFromCache) return
    loadedFromCache.add(id)
    if (id in router.logs) return
    val hit = cache.peek(id)
    if (hit != null) { applyCached(id, hit); return }
    scope.launch {
        val c = cache.load(id) ?: return@launch
        withContext(Dispatchers.Main) {
            if (id in router.logs) return@withContext // a live log arrived while we read
            applyCached(id, c)
            if (id == router.currentId) { router.publish(); scrollToBottom() }
        }
    }
}

private fun VoiceController.applyCached(id: String, c: CachedSession) {
    router.logs[id] = session.dedupe(c.messages.map { it.toChat() })
    router.oldest[id] = c.oldestIndex
    router.hasMore[id] = c.hasMore
    session.recordHeld(id, c.count, c.hash)
}

// persist hands a session's current log (minus live-only SYSTEM notes, which
// aren't part of the server transcript) plus its paging cursor and held digest to
// the cache (keyed by the stable session id), so it survives an app restart and can
// be shown offline. TranscriptCache.save returns immediately — the encode and file
// write happen on a background dispatcher — so this is safe on the UI thread.
internal fun VoiceController.persist(id: String) {
    if (id.isEmpty()) return
    val msgs = session.dedupe(router.logs[id] ?: return)
    router.logs[id] = msgs
    val keep = msgs.filter { it.role != Role.SYSTEM }
    val d = session.heldDigest(id)
    cache.save(id, CachedSession(
        messages = keep.map { it.toCached() },
        oldestIndex = router.oldest[id] ?: (keep.firstOrNull { it.index >= 0 }?.index ?: 0),
        hasMore = router.hasMore[id] ?: false,
        count = d?.first ?: 0,
        hash = d?.second ?: "",
    ))
}

internal fun VoiceController.focusKnownSession(target: DiscoveredInfo, syncServer: Boolean) {
    if (target.sessionId.isBlank()) {
        // No stable handle to key local state by, so this one path stays
        // server-authoritative: don't move focus optimistically, let the server's
        // `attached` frame drive it. Always silent (a tap shouldn't be narrated) —
        // a failure still comes back as `attach_failed`, which silence never
        // suppresses, so this can't strand us mid-switch.
        client?.send(Outbound.attach(target.name, silent = true))
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
    showLog(target.sessionId)
    session.requestFreshHistory(target.sessionId, target.name)
    if (syncServer) client?.send(Outbound.attach(target.name, sessionId = target.sessionId, silent = true))
}

/** Locally bump a session's sidebar metadata (recency + busy cue) the instant a
 *  message arrives, so the list re-sorts and shows "working…" without waiting for
 *  the next `discover` round trip. A no-op if the session isn't in the list yet
 *  (a later discover fills it in). Not written to the disk cache — the authoritative
 *  snapshot still comes from the server's `discovered` frame. */
internal fun VoiceController.touchDiscovered(id: String, busy: Boolean? = null) {
    if (id.isEmpty()) return
    val now = System.currentTimeMillis() / 1000
    var changed = false
    val next = _discovered.value.map { d ->
        if (d.sessionId == id) {
            changed = true
            d.copy(lastActive = maxOf(d.lastActive, now), busy = busy ?: d.busy)
        } else d
    }
    if (changed) _discovered.value = next
}

/** Request the page of history just older than what we hold for session [id]. Shared
 *  by the user's scroll-back (loadOlder) and the reconnect gap-fill in onHistory. The
 *  cursors are id-keyed; the history request itself is addressed by [name]. */
internal fun VoiceController.fetchOlder(id: String, name: String) {
    if (id.isEmpty() || router.hasMore[id] != true || id in loadingOlder) return
    val before = router.oldest[id] ?: return
    loadingOlder.add(id)
    client?.send(Outbound.history(id, name, before))
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
        // Session-scoped chat/log frames: filed by the shared commonMain router.
        is ServerMsg.Say -> router.onSay(msg)
        is ServerMsg.Output -> router.onOutput(msg)
        is ServerMsg.ContextReset -> onContextReset(msg)
        is ServerMsg.Activity -> router.onActivity(msg)
        is ServerMsg.Transcribing -> onTranscribing(msg)
        is ServerMsg.SpeechGate -> _speechGate.value = msg
        is ServerMsg.Files -> router.onFiles(msg)
        is ServerMsg.Diff -> router.onDiff(msg)
        is ServerMsg.RateLimit -> _rateLimit.value = msg.info // plan session-limit readout (sidebar)
        is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report } // opens the usage sheet
        is ServerMsg.Ask -> router.onAsk(msg)
        is ServerMsg.Transcript -> router.onTranscript(msg)
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
        is ServerMsg.AttachFailed -> onAttachFailed(msg)
        is ServerMsg.Detached -> onDetached(msg)
        is ServerMsg.Renamed -> onRenamed(msg)
        is ServerMsg.Notice -> router.onNotice(msg)
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
        is ServerMsg.Err -> router.onErr(msg)
        is ServerMsg.TurnInterrupted -> router.onTurnInterrupted(msg)
        is ServerMsg.TurnStopped -> router.onTurnStopped(msg)
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
    _serverDenoiseAvailable.value = msg.denoise
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

internal fun VoiceController.onContextReset(msg: ServerMsg.ContextReset) {
    router.usageFor(msg.sessionId.ifEmpty { router.currentId }, null) // context cleared → the attached session's status bar returns to 0
    // A clear/compress rotates the session_id server-side and wipes/summarizes the
    // transcript. The rotation bridges OLD id → NEW id by name (the reset is for the
    // attached session): drop the retired old id's rows, re-key the view + attached id
    // to the fresh one, and refetch. An old server omits session_id → meter reset only.
    // The attach-StateFlow/prefs choreography stays here; the router owns the log/keying.
    if (msg.sessionId.isNotEmpty() && _attachedName.value == msg.name) {
        val oldId = _attachedId.value
        _attachedId.value = msg.sessionId
        settings.lastSessionId = msg.sessionId
        val wasViewing = router.currentId == oldId
        dropSessionCache(oldId) // retired id's transcript wiped/summarized: forget rows + digests
        if (wasViewing) showLog(msg.sessionId) // switch the view to the fresh (empty) id
        session.requestFreshHistory(msg.sessionId, msg.name)
    }
}

internal fun VoiceController.onTranscribing(msg: ServerMsg.Transcribing) {
    // A committed hands-free clip is being re-transcribed accurately.
    // Show "transcribing…" instead of flashing back to "listening" until
    // the transcript lands (which flips this to "thinking…").
    if (hfOn) _voiceState.value = VoiceState.TRANSCRIBING
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
    // The echo for a switch we already applied optimistically (focusKnownSession): the
    // view is already this session, so re-running showLog would only re-publish and yank
    // the scroll back to the bottom mid-read. Everything else below is idempotent.
    val alreadyShown = msg.sessionId.isNotBlank() && msg.sessionId == router.currentId
    session.rememberPreviousOnAttach(msg.name, msg.sessionId)
    // Storage is keyed by the stable session_id, so a backend switch (set_agent) that
    // rotates the id and re-emits `attached` just lands on a fresh, empty id and refetches
    // — no stale-row drop dance needed (the old id orphans harmlessly).
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
    if (!alreadyShown) showLog(msg.sessionId)
    // Refetch recent history on (re)attach so a session that produced output
    // while we viewed another one isn't left stale (the server only fans live
    // output to the currently-attached connection). But save data when we can:
    // if the connect-time digest sweep says this session's server hash still
    // equals what our cache holds — and we actually have cached content — the
    // transcript is unchanged, so skip the fetch entirely. Otherwise ask for
    // the recent page, passing the hash we hold so the server can still answer
    // `unchanged` (no bodies) if nothing moved. onHistory dedupes against live.
    session.requestFreshHistory(msg.sessionId, msg.name)
}

/**
 * The server refused an attach: its attachment did **not** move. We switch focus
 * optimistically (so a tap renders in the same frame), which is only safe because
 * this nack exists — otherwise we'd sit on a session the connection was never put
 * on and quietly dictate into whatever it *is* on.
 *
 * Reconcile rather than guess: ignore a nack for anything but the session we're
 * currently showing (a late nack for a session we've already moved off is stale),
 * and otherwise fall back to the most recent still-known session, or to nothing.
 * A fresh `discover` goes out either way so the sidebar stops offering a session
 * this server doesn't have.
 */
internal fun VoiceController.onAttachFailed(msg: ServerMsg.AttachFailed) {
    val mine = (msg.sessionId.isNotEmpty() && msg.sessionId == _attachedId.value) ||
        (msg.sessionId.isEmpty() && msg.name.isNotEmpty() && msg.name == _attachedName.value)
    client?.send(Outbound.discover())
    if (!mine) return
    if (msg.sessionId.isNotEmpty()) dropSessionCache(msg.sessionId)
    val fallback = session.attachHistory(limit = 1).firstOrNull()
    if (fallback != null) {
        focusKnownSession(fallback, syncServer = true)
        _status.value = "couldn't attach — back on ${fallback.name}"
        return
    }
    _attachedId.value = ""
    _attachedName.value = null
    _attachedAgent.value = ""
    _attachedModel.value = ""
    settings.lastSession = ""
    settings.lastSessionId = ""
    showLog("")
    _status.value = "couldn't attach: ${msg.name.ifEmpty { msg.sessionId }}"
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
    // Follow a rename of the session we're attached to so the title bar tracks the
    // sidebar. Match by the stable session id (the title's name may be stale — e.g. a
    // leftover from another server — so a name compare misses); fall back to the old
    // name only when the server sent no id. Storage is id-keyed, so this is a pure
    // title update — no map migration, no history refetch, no meter reseed.
    val mine = if (msg.sessionId.isNotEmpty()) _attachedId.value == msg.sessionId
    else _attachedName.value == msg.old
    if (mine) {
        _attachedName.value = msg.name
        settings.lastSession = msg.name
        _status.value = "attached: ${msg.name}"
    }
}

internal fun VoiceController.onDiscovered(msg: ServerMsg.Discovered) {
    _discovered.value = msg.sessions
    discoveredCache.save(msg.sessions)
    _discoverError.value = ""
    // Sweep transcripts for sessions that no longer exist — this list is the only
    // signal the app gets that a session was deleted server-side. retainOnly is
    // throttled, grace-period'd (this is one server's list, not global truth) and
    // does its work off the main thread, so calling it on every discover is fine.
    cache.retainOnly(msg.sessions.map { it.sessionId }.filter { it.isNotEmpty() }.toSet())
    // Re-derive the attached TITLE from the fresh list by stable id. After a server
    // switch the same session can carry a different name here, leaving the title stale;
    // storage is id-keyed, so this is a title-only update (no map migration).
    if (_attachedId.value.isNotEmpty()) {
        val cur = msg.sessions.find { it.sessionId == _attachedId.value }?.name
        if (cur != null && cur != _attachedName.value) {
            _attachedName.value = cur
            settings.lastSession = cur
            _status.value = "attached: $cur"
        }
    }
}

/** Switch the visible chat to session [key]'s log (stable id, or "" for general). */
internal fun VoiceController.showLog(key: String) {
    if (router.currentId != key) persist(router.currentId) // save what we were viewing (captures live-streamed tail)
    router.currentId = key
    ensureLoaded(key) // fault the cached transcript in from disk if we don't have it live
    router.publish() // reflect the new view's chat + has-more into the StateFlows
    scrollToBottom() // attaching / switching → show the latest (history refresh re-scrolls)
}

// onHistory merges a server-served page of OLDER messages into the session's
// log, ordered chronologically with any live messages, and updates the paging cursor.
internal fun VoiceController.onHistory(msg: ServerMsg.History) {
    val key = msg.sessionId.ifEmpty { router.currentId } // file the page under the stable id
    // `unchanged` answers a top-page request whose have_hash still matched: our
    // cached transcript is current, so keep it untouched and just refresh the
    // stored digest (both held and server) so future freshness checks stand.
    if (msg.unchanged) {
        if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
        loadingOlder.remove(key)
        router.logs[key]?.let { cleaned ->
            val deduped = session.dedupe(cleaned)
            router.logs[key] = deduped
            if (key == router.currentId) _chat.value = deduped
        }
        persist(key)
        return
    }
    val wasLoadOlder = key in loadingOlder // else it's the top page (on (re)attach)
    val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts, id = it.id, turnStats = it.turnStats) }
    // The shared merge (keep-rule, chronological order, de-dup, cursor, gap watermark) —
    // identical on both clients; only the storage side effects below are Android's.
    val merged = session.mergeHistory(router.logs[key] ?: emptyList(), hist, wasLoadOlder, msg.more)
    router.logs[key] = merged.messages
    merged.oldest?.let { router.oldest[key] = it }
    router.hasMore[key] = merged.hasMore
    loadingOlder.remove(key)
    // Record the chain digest this page belongs to and persist the merged log, so
    // the cache is current on disk and a later reattach can short-circuit the fetch.
    if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
    persist(key)
    // Reconnect gap-fill: the reattach top page is only the newest slice, so if the
    // session advanced by more than a page while we were away, a hole is left between
    // what we still held and this page's oldest index. The shared merge marks the
    // watermark; keep paging older until we reconnect with it (or hit the start) so the
    // whole gap backfills instead of only the newest page.
    merged.gapTarget?.let { bridgeTo[key] = it }
    bridgeTo[key]?.let { target ->
        val oldest = router.oldest[key]
        if (oldest != null && oldest > target + 1 && router.hasMore[key] == true) {
            fetchOlder(key, msg.name) // still a hole above the watermark — keep paging
        } else {
            bridgeTo.remove(key) // reconnected with what we had (or reached the start)
        }
    }
    if (key == router.currentId) {
        _chat.value = router.logs[key] ?: emptyList()
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

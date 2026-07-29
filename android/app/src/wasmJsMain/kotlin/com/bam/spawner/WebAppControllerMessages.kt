package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ServerMsg

// Split out of WebAppController.kt to keep the giant onMessage dispatcher and its
// message/chat/session-state helpers in one focused file. Pure relocation — see
// WebAppController.kt for the class fields these extensions read/mutate.
//
// The per-session chat/log filing (addChat, publish, dropSessionCache,
// attachUsageToLastClaude, the activityFor/usageFor/askFor attached-gate, and the
// bodies of the session-scoped frames) now lives in the shared commonMain
// InboundRouter (`controller.router`). What stays here is the platform-specific
// glue: the attach/detach/context-reset choreography (which writes the attach
// StateFlows + prefs) and the index-sorted in-memory History merge.

// touchDiscovered locally bumps a session's sidebar metadata (recency + busy cue) the
// instant a message arrives, so the list re-sorts and shows "working…" without waiting
// for the next `discover` round trip. A no-op if the session isn't in the list yet
// (a later discover fills it in). Never persisted — the authoritative snapshot still
// comes from the server's `discovered` frame. It mutates the discovered StateFlow, so it
// stays controller-owned and the router reaches it through InboundRouter.Host.touchDiscovered.
internal fun WebAppController.touchDiscovered(id: String, busy: Boolean? = null) {
    if (id.isEmpty()) return
    val now = nowEpochSeconds()
    var changed = false
    val next = _discovered.value.map { d ->
        if (d.sessionId == id) {
            changed = true
            d.copy(lastActive = maxOf(d.lastActive, now), busy = busy ?: d.busy)
        } else d
    }
    if (changed) _discovered.value = next
}

internal fun WebAppController.focusKnownSession(target: DiscoveredInfo, syncServer: Boolean) {
    if (target.sessionId.isBlank()) {
        client?.send(Outbound.attach(target.name, silent = syncServer))
        return
    }
    session.rememberPreviousIfSwitching(target.sessionId)
    _activity.value = ""
    _pending.value = ""
    _lastTurnUsage.value = null
    _attachedId.value = target.sessionId
    _attachedName.value = target.name
    _attachedAgent.value = target.agent
    _attachedModel.value = target.model
    prefs.lastSession = target.name
    prefs.lastSessionId = target.sessionId
    _status.value = "attached: ${target.name}"
    router.currentId = target.sessionId
    router.publish()
    _scrollTick.value = _scrollTick.value + 1
    session.requestFreshHistory(target.sessionId, target.name)
    if (syncServer) client?.send(Outbound.attach(target.name, sessionId = target.sessionId, silent = true))
}

internal fun WebAppController.roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE


internal fun WebAppController.onMessage(msg: ServerMsg) {
    when (msg) {
        is ServerMsg.HelloOk -> {
            _status.value = "connected"
            if (msg.whisperModel.isNotBlank()) { _whisperModel.value = msg.whisperModel; prefs.whisperModel = msg.whisperModel }
            // Unconditional: "" is meaningful (no fast server configured there).
            _whisperFastModel.value = msg.whisperModelFast
            prefs.whisperFastModel = msg.whisperModelFast
            _whisperModels.value = msg.whisperModels
            _whisperModelsLocal.value = msg.whisperModelsLocal
            _serverTtsAvailable.value = msg.tts
            if (msg.tts) client?.send(Outbound.ttsVoices()) // fetch the voice-picker catalogue
            _serverDenoiseAvailable.value = msg.denoise
            discover()
            client?.send(Outbound.digest()) // validate the in-memory transcript cache (bodies-free)
            if (prefs.lastSession.isNotBlank()) {
                client?.send(Outbound.attach(prefs.lastSession, prefs.lastSessionId, silent = true))
            }
        }
        is ServerMsg.WhisperModel -> {
            if (msg.model.isNotBlank()) { _whisperModel.value = msg.model; prefs.whisperModel = msg.model }
            _whisperFastModel.value = msg.fastModel
            prefs.whisperFastModel = msg.fastModel
            if (msg.models.isNotEmpty()) _whisperModels.value = msg.models
            _whisperModelsLocal.value = msg.local
        }
        is ServerMsg.WhisperDownload -> {
            _whisperDownload.value =
                if (msg.done && msg.error.isBlank()) null
                else WhisperDownloadInfo(msg.model, msg.fast, msg.received, msg.total, msg.done, msg.error)
        }
        // Session-scoped chat/log frames: filed by the shared commonMain router.
        is ServerMsg.Say -> router.onSay(msg)
        is ServerMsg.Output -> router.onOutput(msg)
        is ServerMsg.StopSpeaking -> { cancelServerSpeech(); cancelSpeech(); _speaking.value = false }
        is ServerMsg.SpeakAudio -> onSpeakAudio(msg)
        is ServerMsg.SpeakEnd -> onSpeakEnd(msg)
        is ServerMsg.TtsVoices -> if (msg.error.isEmpty()) {
            _ttsVoices.value = msg.voices
            _ttsVoiceDefault.value = msg.defaultVoice
        }
        is ServerMsg.SpeechMode -> prefs.summaryOnlySpeech = msg.summaryOnly // voice toggle mirrors the audio-settings switch
        is ServerMsg.ContextReset -> {
            router.usageFor(router.keyOf(msg.sessionId), null) // context cleared → the attached session's status bar returns to 0
            // A clear/compress rotates the session_id server-side and wipes/summarizes the
            // transcript. The rotation bridges OLD id → NEW id by name (the reset is for the
            // attached session): drop the retired old id's rows, re-key the view + attached id
            // to the fresh one, and refetch. An old server omits session_id → meter reset only.
            // The attach-StateFlow/prefs choreography stays here; the router owns the log/keying.
            if (msg.sessionId.isNotEmpty() && _attachedName.value == msg.name) {
                val oldId = _attachedId.value
                _attachedId.value = msg.sessionId
                prefs.lastSessionId = msg.sessionId
                val wasViewing = router.currentId == oldId
                if (wasViewing) router.currentId = msg.sessionId
                router.dropSessionCache(oldId) // retired id's transcript wiped/summarized: forget rows + digests
                if (wasViewing) router.publish() // reflect the fresh (empty) new-id view
                session.requestFreshHistory(msg.sessionId, msg.name)
            }
        }
        is ServerMsg.Activity -> router.onActivity(msg)
        is ServerMsg.Transcribing -> _micText.value = "transcribing…" // committed clip being re-transcribed
        is ServerMsg.Files -> router.onFiles(msg)
        is ServerMsg.Diff -> router.onDiff(msg)
        is ServerMsg.RateLimit -> _rateLimit.value = msg.info
        is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report }
        is ServerMsg.Ask -> router.onAsk(msg)
        is ServerMsg.Transcript -> router.onTranscript(msg)
        is ServerMsg.Attached -> {
            session.rememberPreviousOnAttach(msg.name, msg.sessionId)
            // Storage is keyed by the stable session_id, so a backend switch (set_agent)
            // that rotates the id and re-emits `attached` just lands on a fresh, empty id
            // and refetches — no stale-row drop dance needed (the old id orphans harmlessly).
            // The attach-StateFlow/prefs choreography stays here; the router owns the log/keying.
            _activity.value = ""
            _attachedId.value = msg.sessionId
            _attachedName.value = msg.name
            _attachedAgent.value = msg.agent; _attachedModel.value = msg.model
            prefs.lastSession = msg.name; prefs.lastSessionId = msg.sessionId
            _status.value = "attached: ${msg.name}"
            // Anchor the cache-warm countdown to the last turn's real age (from
            // `usage_at`), not to now — otherwise a restart shows a fresh 5-min
            // window for a session whose cache went cold while we were away.
            if (msg.usage != null) {
                val ageMs = if (msg.usageAt > 0) (nowEpochSeconds() - msg.usageAt) * 1000 else Long.MAX_VALUE
                _lastTurnUsage.value = TurnUsageInfo(msg.usage, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))
            }
            router.currentId = msg.sessionId
            router.publish()
            router.loadingOlder = false
            session.requestFreshHistory(msg.sessionId, msg.name)
        }
        is ServerMsg.Detached -> {
            session.rememberPrevious()
            _attachedId.value = ""; _attachedName.value = null
            _attachedAgent.value = ""; _attachedModel.value = ""
            prefs.lastSession = ""; prefs.lastSessionId = ""
            _status.value = "connected"; router.currentId = ""; router.publish()
        }
        is ServerMsg.Renamed -> {
            // Storage is id-keyed, so a rename is purely a title change — no map re-keying and
            // no per-session log/keying, so the whole (attach-title) body stays in the controller.
            if (msg.old == _attachedName.value || (msg.sessionId.isNotBlank() && msg.sessionId == _attachedId.value)) {
                _attachedName.value = msg.name; prefs.lastSession = msg.name
                _status.value = "attached: ${msg.name}"
            }
        }
        is ServerMsg.History -> onHistory(msg)
        is ServerMsg.Discovered -> {
            _discovered.value = msg.sessions
            _discoverError.value = ""
            // Re-derive the attached TITLE from the fresh list by stable id. After a server
            // switch the same session can carry a different name here, leaving the title
            // stale; storage is id-keyed, so this is a title-only update (no map migration).
            if (_attachedId.value.isNotEmpty()) {
                val cur = msg.sessions.find { it.sessionId == _attachedId.value }?.name
                if (cur != null && cur != _attachedName.value) {
                    _attachedName.value = cur
                    prefs.lastSession = cur
                    _status.value = "attached: $cur"
                }
            }
        }
        is ServerMsg.Listing -> _listing.value = msg
        is ServerMsg.FileSaved -> _fileSaved.tryEmit(msg.path)
        is ServerMsg.FileData -> _fileData.tryEmit(msg)
        is ServerMsg.HostList, is ServerMsg.IdentityList,
        is ServerMsg.Agents, is ServerMsg.Profiles,
        is ServerMsg.SpokenTokens -> catalogues.apply(msg)
        is ServerMsg.Actions -> _spokenActions.value = msg.actions
        is ServerMsg.Settings -> { catalogues.apply(msg); mirrorSettingsToPrefs() }
        is ServerMsg.Digests -> {
            // Connect-time server-truth sweep. No longer consulted: transcript freshness
            // is checked per-attach via `have_hash` → `unchanged` (see requestFreshHistory),
            // which — unlike a cached connect snapshot — can't go stale for a session we're
            // detached from and so silently drop its messages. Kept as a protocol no-op.
        }
        is ServerMsg.ReadLast -> onReadLast(msg.count)
        is ServerMsg.Pending -> _pending.value = msg.text // live hands-free draft (the web has VAD hands-free too)
        is ServerMsg.Err -> router.onErr(msg)
        is ServerMsg.TurnInterrupted -> router.onTurnInterrupted(msg)
        is ServerMsg.TurnStopped -> router.onTurnStopped(msg)
        // Phone-only voice surfaces with no web analogue — explicit, documented
        // no-ops so the omission is intentional, not an accidental gap:
        is ServerMsg.Calibration -> {} // detection-model mic calibration; the web has no calibration UI
        is ServerMsg.Dialog -> {} // server-side voice-dialog state machine (spawn "where?" etc.); its spoken prompts already reach the web via `say`
        is ServerMsg.Unknown -> {} // unrecognized wire type: ignore rather than crash
    }
}

// onReadLast re-reads (TTS) the last `count` Claude replies in the current view —
// the `read last` voice command; the web speaks them the same way the phone does.
internal fun WebAppController.onReadLast(count: Int) {
    val claude = _chat.value.filter { it.role == Role.CLAUDE }.takeLast(count.coerceAtLeast(1))
    if (claude.isEmpty()) speak("nothing to read yet")
    else speak(claude.joinToString(". … ") { it.text })
    _scrollTick.value = _scrollTick.value + 1
}

// onHistory merges an inbound history page into the router's in-memory transcript store.
// This index-sorted, in-memory merge is the web's platform-specific strategy (Android's is
// a disk-backed timestamp merge with reconnect gap-fill), so it stays in the controller and
// drives the shared router state directly rather than moving into InboundRouter.
internal fun WebAppController.onHistory(msg: ServerMsg.History) {
    val key = msg.sessionId.ifEmpty { router.currentId } // file the page under the stable id
    // `unchanged` answers a top-page freshness check whose have_hash still matched:
    // our in-memory transcript is current, so keep it untouched and just refresh the
    // stored digest so future freshness checks stand.
    if (msg.unchanged) {
        if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
        router.loadingOlder = false
        return
    }
    val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts) }
    val existing = router.logs[key] ?: emptyList()
    router.logs[key] = if (router.loadingOlder) {
        // Prepend older page, keeping the live tail; the shared index-aware de-dup
        // collapses any live chunk already landed as an indexed history row.
        session.dedupe(hist + existing.filter { it.index < 0 || it.index > (hist.lastOrNull()?.index ?: -1) })
            .sortedBy { if (it.index >= 0) it.index else Int.MAX_VALUE }
    } else {
        // The top page is the authoritative transcript tail — but PRESERVE what it
        // doesn't cover, like the Android client: indexed rows from older pages we
        // already loaded, and live (index < 0) rows whose text isn't in the page
        // yet (a turn still streaming — or a backend with NO readable transcript,
        // e.g. Antigravity, whose pages are always empty; a naked replace here
        // wiped the only copy of those conversations on every reconnect).
        val histIdx = hist.mapNotNull { m -> m.index.takeIf { i -> i >= 0 } }.toSet()
        val histTexts = hist.map { it.role to it.text }.toSet()
        val kept = existing.filter {
            (it.index < 0 && (it.role to it.text) !in histTexts) ||
                (it.index >= 0 && it.index !in histIdx)
        }
        session.dedupe((hist + kept).sortedBy { if (it.index >= 0) it.index else Int.MAX_VALUE })
    }
    router.loadingOlder = false
    router.oldest[key] = hist.firstOrNull()?.index ?: (router.oldest[key] ?: 0)
    router.hasMore[key] = msg.more
    // Record the chain digest this page belongs to so a later reattach can
    // short-circuit the fetch when the server hash still matches what we hold.
    if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
    if (key == router.currentId) { router.publish(); _scrollTick.value = _scrollTick.value + 1 }
}

// mirrorSettingsToPrefs folds the inbound shared-settings catalogue into the
// device-local Prefs the settings UI seeds from, so a change synced from another
// client (or the server) is reflected here. Whisper models drive their own StateFlows
// via the `whisper_model` broadcast; here we mirror only the config scalars.
internal fun WebAppController.mirrorSettingsToPrefs() {
    catalogues.settingValue("warm_compress")?.let { prefs.warmCompress = it == "true" }
    catalogues.settingValue("auto_compress")?.let { prefs.autoCompress = it == "true" }
    catalogues.settingValue("auto_compress_threshold")?.let { prefs.autoCompressThreshold = it.toIntOrNull() ?: prefs.autoCompressThreshold }
    catalogues.settingValue("summary_only")?.let { prefs.summaryOnlySpeech = it == "true" }
}

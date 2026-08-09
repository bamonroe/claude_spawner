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
// StateFlows + prefs) and the in-memory History merge (chronological, via the shared
// orderByTimestamp).

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
        // No stable handle to key local state by, so this path stays server-
        // authoritative: don't move focus optimistically, let `attached` drive it.
        // Silent only suppresses the spoken line — a failure still comes back as
        // `attach_failed`, so this can't strand us mid-switch.
        client?.send(Outbound.attach(target.name, silent = true))
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
            listingCache.clear() // a fresh connection may be a different host/filesystem
            if (msg.whisperModel.isNotBlank()) { _whisperModel.value = msg.whisperModel; prefs.whisperModel = msg.whisperModel }
            // Unconditional: "" is meaningful (no fast server configured there).
            _whisperFastModel.value = msg.whisperModelFast
            prefs.whisperFastModel = msg.whisperModelFast
            _whisperModels.value = msg.whisperModels
            _whisperModelsLocal.value = msg.whisperModelsLocal
            _serverTtsAvailable.value = msg.tts
            if (msg.tts) client?.send(Outbound.ttsVoices()) // fetch the voice-picker catalogue
            _serverDenoiseAvailable.value = msg.denoise
            // The server re-states the pending bit on reconnect (we missed any
            // restart_status while the socket was down); nothing is building yet.
            _restartPending.value = msg.restartPending
            _restartBuilding.value = false
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
            // transcript. The frame is BROADCAST to every device and names the retirement
            // explicitly (old_id → session_id): remap everything keyed by the old id even
            // when this device is attached elsewhere — keeping it meant later attaching by
            // a handle no session owns. An old server omits old_id → fall back to the
            // by-name bridge (helps only the attached device, as before); an omitted
            // session_id → meter reset only.
            val oldId = msg.oldId.ifEmpty { if (_attachedName.value == msg.name) _attachedId.value else "" }
            if (msg.sessionId.isNotEmpty() && oldId.isNotEmpty() && oldId != msg.sessionId) {
                session.remapId(oldId, msg.sessionId, msg.preserved) // swap/palette history + held digest
                val wasViewing = router.currentId == oldId
                if (_attachedId.value == oldId) {
                    _attachedId.value = msg.sessionId
                    prefs.lastSessionId = msg.sessionId
                }
                if (wasViewing) router.currentId = msg.sessionId
                // `preserved` (a clear) leaves the rendered log byte-identical — the
                // retired id is only appended to the chain — so re-key the cached rows
                // and keep rendering; a compress rewrote the context, so drop them.
                if (msg.preserved) router.remapSessionCache(oldId, msg.sessionId)
                else router.dropSessionCache(oldId) // transcript summarized: forget rows + digests
                bridgeTo.remove(oldId) // no gap left to bridge on a retired id
                if (wasViewing) {
                    router.publish() // keep showing the (re-keyed) log; empty after a compress
                    session.requestFreshHistory(msg.sessionId, msg.name) // have_hash → `unchanged` after a clear
                }
            }
        }
        is ServerMsg.Activity -> router.onActivity(msg)
        is ServerMsg.ContextUsage -> router.onContextUsage(msg)
        is ServerMsg.Transcribing -> _micText.value = "transcribing…" // committed clip being re-transcribed
        is ServerMsg.SpeechGate -> _speechGate.value = msg
        is ServerMsg.RestartStatus -> restartIndicators(msg).let { (building, pending) ->
            _restartBuilding.value = building
            _restartPending.value = pending
        }
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
            // Skip the re-publish when this is just the echo of a switch we already
            // applied optimistically in focusKnownSession — the view is already here.
            if (router.currentId != msg.sessionId) {
                router.currentId = msg.sessionId
                router.publish()
            }
            router.loadingOlder = false
            session.requestFreshHistory(msg.sessionId, msg.name)
        }
        // The attach didn't happen — the server's attachment is unchanged. We move
        // focus optimistically, so reconcile back rather than sit on a session this
        // connection was never put on. Stale nacks (for a session we've since left)
        // are ignored; a fresh `discover` goes out either way.
        is ServerMsg.AttachFailed -> {
            val mine = (msg.sessionId.isNotEmpty() && msg.sessionId == _attachedId.value) ||
                (msg.sessionId.isEmpty() && msg.name.isNotEmpty() && msg.name == _attachedName.value)
            client?.send(Outbound.discover())
            if (mine) {
                val fallback = session.attachHistory(limit = 1).firstOrNull()
                if (fallback != null) {
                    focusKnownSession(fallback, syncServer = true)
                    _status.value = "couldn't attach — back on ${fallback.name}"
                } else {
                    _attachedId.value = ""; _attachedName.value = null
                    _attachedAgent.value = ""; _attachedModel.value = ""
                    prefs.lastSession = ""; prefs.lastSessionId = ""
                    router.currentId = ""; router.publish()
                    _status.value = "couldn't attach: ${msg.name.ifEmpty { msg.sessionId }}"
                }
            }
        }
        is ServerMsg.Detached -> {
            session.rememberPrevious()
            _attachedId.value = ""; _attachedName.value = null
            _attachedAgent.value = ""; _attachedModel.value = ""
            prefs.lastSession = ""; prefs.lastSessionId = ""
            _status.value = "connected"; router.currentId = ""; router.publish()
        }
        is ServerMsg.Notice -> router.onNotice(msg)
        is ServerMsg.Renamed -> {
            // Storage is id-keyed, so a rename is purely a title change — no map re-keying and
            // no per-session log/keying, so the whole (attach-title) body stays in the controller.
            // Match by the stable session id when the server sent one (a name compare can hit a
            // stale/duplicate title and rename the wrong session); fall back to the old name only
            // when there's no id — parity with the Android onRenamed precedence.
            val mine = if (msg.sessionId.isNotBlank()) msg.sessionId == _attachedId.value
            else msg.old == _attachedName.value
            if (mine) {
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
        is ServerMsg.Listing -> { listingCache.put(msg); _listing.value = msg }
        is ServerMsg.FileSaved -> _fileSaved.tryEmit(msg.path)
        is ServerMsg.FileData -> _fileData.tryEmit(msg)
        is ServerMsg.HostList, is ServerMsg.IdentityList,
        is ServerMsg.Agents, is ServerMsg.Profiles,
        is ServerMsg.SpokenTokens -> catalogues.apply(msg)
        is ServerMsg.ShellCommands -> catalogues.apply(msg)
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

// onHistory folds an inbound history page into the router's in-memory transcript store. The
// merge itself — the keep/drop rule, the chronological ordering, the de-dup, the paging cursor
// and the reconnect gap-fill watermark — is the shared SessionSync.mergeHistory, identical to
// the phone's; only the storage (in memory here, disk-backed there) is platform-specific.
/** Request the page of history just older than what we hold for session [id] — the web's
 *  counterpart to the phone's fetchOlder, driving the reconnect gap-fill in onHistory. The
 *  cursors are id-keyed; the history request itself is addressed by [name]. */
internal fun WebAppController.fetchOlder(id: String, name: String, limit: Int = Outbound.HISTORY_PAGE) {
    if (id.isEmpty() || router.hasMore[id] != true || router.loadingOlder) return
    val before = router.oldest[id] ?: return
    router.loadingOlder = true
    client?.send(Outbound.history(id, name, before, limit = limit))
}

internal fun WebAppController.onHistory(msg: ServerMsg.History) {
    val key = msg.sessionId.ifEmpty { router.currentId } // file the page under the stable id
    // `unchanged` answers a top-page freshness check whose have_hash still matched:
    // our in-memory transcript is current, so keep it untouched and just refresh the
    // stored digest so future freshness checks stand.
    if (msg.unchanged) {
        if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
        router.loadingOlder = false
        // Re-run the shared de-dup over what we hold (as the phone does): the held log may
        // carry a live copy of a row that has since settled, and no page is coming to fold it.
        router.logs[key]?.let { held ->
            router.logs[key] = session.dedupe(held)
            if (key == router.currentId) router.publish()
        }
        return
    }
    val wasLoadOlder = router.loadingOlder // else it's the top page (on (re)attach)
    val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts, id = it.id, turnStats = it.turnStats) }
    val merged = session.mergeHistory(router.logs[key] ?: emptyList(), hist, wasLoadOlder, msg.more, msg.delta)
    router.logs[key] = merged.messages
    router.loadingOlder = false
    merged.oldest?.let { router.oldest[key] = it }
    merged.hasMore?.let { router.hasMore[key] = it }
    // Hold this page's prefix digest to echo back next time (parity with the phone); a
    // page-back's covers only the log through the older cursor, so it isn't worth keeping.
    if (!wasLoadOlder) session.recordPrefix(key, msg.prefixCount, msg.prefixHash)
    // Record the chain digest this page belongs to so a later reattach can
    // short-circuit the fetch when the server hash still matches what we hold.
    if (msg.hash.isNotEmpty()) session.recordSynced(key, msg.count, msg.hash)
    // Reconnect gap-fill (parity with the phone): a reattach top page is only the newest
    // slice, so if the session advanced by more than a page while we were away, keep paging
    // older until we reconnect with what we still held — or reach the start of the transcript.
    merged.gapTarget?.let { bridgeTo[key] = it }
    bridgeTo[key]?.let { target ->
        val oldest = router.oldest[key]
        if (oldest != null && oldest > target + 1 && router.hasMore[key] == true) {
            // Big pages here: each round trip is serial, so a wide gap backfills in a
            // few fetches instead of dozens (see Outbound.HISTORY_GAP_PAGE).
            fetchOlder(key, msg.name, Outbound.HISTORY_GAP_PAGE)
        } else {
            bridgeTo.remove(key) // reconnected with what we had (or reached the start)
        }
    }
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

package com.bam.spawner

import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ServerMsg

// Split out of WebAppController.kt to keep the giant onMessage dispatcher and its
// message/chat/session-state helpers in one focused file. Pure relocation — see
// WebAppController.kt for the class fields these extensions read/mutate.

// dropSessionCache forgets every name-keyed trace of a session's transcript (in-memory
// log + paging cursors + held/server digests) so the next history fetch rebuilds it from
// scratch. Used when a clear/compress OR a same-name session_id rotation wipes the
// session server-side: the old rows carry stale indexes and merging a fresh page over
// them would duplicate, so discard wholesale and refetch. Mirror of Android's method.
internal fun WebAppController.dropSessionCache(name: String) {
    logs.remove(name)
    hasMore.remove(name)
    oldest.remove(name)
    session.drop(name) // held + server digests
    if (name == currentKey) publish()
}

/** Locally bump a session's sidebar metadata (recency + busy cue) the instant a
 *  message arrives, so the list re-sorts and shows "working…" without waiting for
 *  the next `discover` round trip. A no-op if the session isn't in the list yet
 *  (a later discover fills it in). Never persisted — the authoritative snapshot
 *  still comes from the server's `discovered` frame. */
internal fun WebAppController.touchDiscovered(name: String, busy: Boolean? = null) {
    if (name.isEmpty()) return
    val now = nowEpochSeconds()
    var changed = false
    val next = _discovered.value.map { d ->
        if (d.name == name) {
            changed = true
            d.copy(lastActive = maxOf(d.lastActive, now), busy = busy ?: d.busy)
        } else d
    }
    if (changed) _discovered.value = next
}


internal fun WebAppController.publish() {
    _chat.value = logs[currentKey] ?: emptyList()
    _hasMoreHistory.value = hasMore[currentKey] ?: false
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
    currentKey = target.name
    publish()
    _scrollTick.value = _scrollTick.value + 1
    session.requestFreshHistory(target.name)
    if (syncServer) client?.send(Outbound.attach(target.name, sessionId = target.sessionId, silent = true))
}

internal fun WebAppController.addChat(role: Role, text: String, usage: com.bam.spawner.net.TokenUsage? = null, key: String = currentKey) {
    val now = nowEpochSeconds()
    // Reconcile on the LIVE path (see SessionSync.dedupe): a hands-free utterance
    // streams a live draft/echo row and then lands the committed `transcript` as a
    // second identical live row (index==-1 both), which nothing collapsed until a
    // reattach. Deduping here drops that adjacent duplicate as it's appended.
    logs[key] = session.dedupe(
        (logs[key] ?: emptyList()) + ChatMessage(role, text, usage = usage, ts = now)
    ).takeLast(2000)
    if (key == currentKey) {
        publish()
        _scrollTick.value = _scrollTick.value + 1
    }
}

internal fun WebAppController.roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE


internal fun WebAppController.onMessage(msg: ServerMsg) {
    when (msg) {
        is ServerMsg.HelloOk -> {
            _status.value = "connected"
            if (msg.whisperModel.isNotBlank()) _whisperModel.value = msg.whisperModel
            // Unconditional: "" is meaningful (no fast server configured there).
            _whisperFastModel.value = msg.whisperModelFast
            prefs.whisperFastModel = msg.whisperModelFast
            _whisperModels.value = msg.whisperModels
            _whisperModelsLocal.value = msg.whisperModelsLocal
            _serverTtsAvailable.value = msg.tts
            if (msg.tts) client?.send(Outbound.ttsVoices()) // fetch the voice-picker catalogue
            discover()
            client?.send(Outbound.digest()) // validate the in-memory transcript cache (bodies-free)
            if (prefs.lastSession.isNotBlank()) {
                client?.send(Outbound.attach(prefs.lastSession, prefs.lastSessionId, silent = true))
            }
        }
        is ServerMsg.WhisperModel -> {
            if (msg.model.isNotBlank()) _whisperModel.value = msg.model
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
        is ServerMsg.Say -> {
            _activity.value = ""
            // A turn-terminal say (compress done) can be redelivered buffered on
            // reconnect — its turn id drops the repeat. Breadcrumb says have no id.
            if (!session.terminalSeen(currentKey, msg.turn)) {
                addChat(Role.SYSTEM, msg.text); speak(msg.text)
            }
        }
        is ServerMsg.Output -> {
            _activity.value = ""
            // Summary-only: beep through intermediate steps, speak only the final result.
            val summaryOnly = prefs.summaryOnlySpeech
            touchDiscovered(msg.name, busy = msg.chunk) // reorder + working cue live
            if (msg.chunk) {
                streamedSessions.add(msg.name)
                session.noteChunk(msg.name, msg.turn)
                addChat(Role.CLAUDE, msg.text, key = msg.name)
                if (msg.name == currentKey) {
                    if (summaryOnly) {
                        // Speak the first N replies of the turn aloud; beep the rest.
                        val spoken = spokenReplyCounts.getOrElse(msg.name) { 0 }
                        if (spoken < prefs.speakInitialReplies) {
                            spokenReplyCounts[msg.name] = spoken + 1
                            speak(msg.text)
                            session.noteSpokenChunk(msg.name, msg.text, msg.turn)
                        } else {
                            webBeep()
                        }
                    } else {
                        speak(msg.text)
                        session.noteSpokenChunk(msg.name, msg.text, msg.turn)
                    }
                }
            } else {
                // Same id-keyed close reconciliation as the Android controller:
                // redelivered = this close's turn was already decided on (buffered
                // resend / doubled close); streamed = its chunks reached us, by id
                // even when the legacy flag was cleared mid-turn. Query the ids
                // BEFORE shouldSpeakClose — that call records them.
                val redelivered = session.closeSeen(msg.name, msg.turn)
                val streamed = streamedSessions.remove(msg.name) ||
                    session.closeStreamed(msg.name, msg.turn)
                spokenReplyCounts.remove(msg.name) // new turn restarts the initial-reply count
                val wantSpeak = session.shouldSpeakClose(msg.name, msg.text, summaryOnly, msg.turn)
                // A live bubble for this reply already exists when the turn streamed, but
                // also when a duplicate closing Output arrives for the same turn (backend
                // double-emit, or streamedSessions cleared mid-turn) — that second close
                // is what appended a second identical bubble. Reuse it in either case.
                val lastClaude = logs[msg.name]?.lastOrNull { it.role == Role.CLAUDE }
                val haveLiveBubble = lastClaude != null && lastClaude.index < 0 &&
                    lastClaude.text.trim() == msg.text.trim()
                if (!streamed && !haveLiveBubble && !redelivered) {
                    addChat(Role.CLAUDE, msg.text, msg.usage, key = msg.name)
                }
                else {
                    if (msg.usage != null) attachUsageToLastClaude(msg.name, msg.usage)
                }
                if (wantSpeak && msg.name == currentKey) speak(msg.text)
                // Anchor the cache-warm countdown to the turn's real completion
                // time (usage_at), not to when a buffered reply reached us.
                msg.usage?.let { u ->
                    val ageMs = if (msg.usageAt > 0) (nowEpochSeconds() - msg.usageAt) * 1000 else 0L
                    _lastTurnUsage.value = TurnUsageInfo(u, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))
                }
            }
        }
        is ServerMsg.StopSpeaking -> { cancelServerSpeech(); cancelSpeech(); _speaking.value = false }
        is ServerMsg.SpeakAudio -> onSpeakAudio(msg)
        is ServerMsg.SpeakEnd -> onSpeakEnd(msg)
        is ServerMsg.TtsVoices -> if (msg.error.isEmpty()) {
            _ttsVoices.value = msg.voices
            _ttsVoiceDefault.value = msg.defaultVoice
        }
        is ServerMsg.SpeechMode -> prefs.summaryOnlySpeech = msg.summaryOnly // voice toggle mirrors the audio-settings switch
        is ServerMsg.ContextReset -> {
            _lastTurnUsage.value = null
            // A clear/compress rotates the session_id server-side and wipes/
            // summarizes the transcript. The rotated id now rides only on this
            // message (the server no longer re-emits `attached`): re-key the
            // attached id, drop the now-stale cached rows for this session, and
            // refetch fresh history. An old server omits session_id → meter reset only.
            if (msg.sessionId.isNotEmpty()) {
                if (_attachedName.value == msg.name) {
                    _attachedId.value = msg.sessionId
                    prefs.lastSessionId = msg.sessionId
                }
                dropSessionCache(msg.name) // rotated id's transcript wiped/summarized: forget rows + digests
                client?.send(Outbound.history(msg.name, null))
            }
        }
        is ServerMsg.Activity -> { _activity.value = msg.text; touchDiscovered(currentKey, busy = true) }
        is ServerMsg.Transcribing -> _micText.value = "transcribing…" // committed clip being re-transcribed
        is ServerMsg.Files -> if (msg.files.isNotEmpty()) {
            addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "))
            if (prefs.summaryOnlySpeech) webBeep() // intermediate step → beep like the rest
        }
        is ServerMsg.Diff -> {
            addChat(Role.SYSTEM, "📊 diff:\n${msg.text}")
            if (prefs.summaryOnlySpeech) webBeep()
        }
        is ServerMsg.RateLimit -> _rateLimit.value = msg.info
        is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report }
        is ServerMsg.Ask -> {
            _activity.value = ""; streamedSessions.remove(msg.name); spokenReplyCounts.remove(msg.name)
            touchDiscovered(msg.name, busy = false) // turn-terminal → clear the working cue
            // An ask is a turn-terminal and can be redelivered buffered on reconnect
            // — keyed by its turn id; drop a repeat instead of re-presenting it.
            if (!session.terminalSeen(msg.name, msg.turn)) {
                _ask.value = msg.questions
                addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q }, key = msg.name)
            }
        }
        is ServerMsg.Transcript -> {
            _ask.value = null
            (_attachedName.value ?: currentKey).takeIf { it.isNotEmpty() }?.let { streamedSessions.remove(it); spokenReplyCounts.remove(it); session.noteTurnStart(it) }
            // The committed transcript supersedes the live hands-free draft — clear it
            // so the utterance isn't shown as both a draft and a committed bubble.
            _pending.value = ""
            addChat(Role.USER, msg.text)
            touchDiscovered(currentKey, busy = true) // dictation submitted → session is now working
        }
        is ServerMsg.Attached -> {
            session.rememberPreviousOnAttach(msg.name, msg.sessionId)
            // A backend switch (set_agent) rotates the session_id but keeps the name and
            // re-emits `attached` (not context_reset). If this is that rotation of the
            // session we're already on — same name, different id — the rows we hold are
            // the wiped old backend's, so drop them (+ digests) before requesting history
            // below, like context_reset. Reads the still-held id/name, so run before we
            // overwrite them. A same-id re-attach drops nothing.
            session.onAttachRotation(msg.name, msg.sessionId)
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
            currentKey = msg.name
            publish()
            loadingOlder = false
            session.requestFreshHistory(msg.name)
        }
        is ServerMsg.Detached -> {
            session.rememberPrevious()
            _attachedId.value = ""; _attachedName.value = null
            _attachedAgent.value = ""; _attachedModel.value = ""
            prefs.lastSession = ""; prefs.lastSessionId = ""
            _status.value = "connected"; currentKey = ""; publish()
        }
        is ServerMsg.Renamed -> {
            if (msg.old == _attachedName.value || (msg.sessionId.isNotBlank() && msg.sessionId == _attachedId.value)) {
                logs[msg.name] = logs.remove(msg.old) ?: emptyList()
                session.migrate(msg.old, msg.name) // held + server digests follow the rename
                if (currentKey == msg.old) currentKey = msg.name
                _attachedName.value = msg.name; prefs.lastSession = msg.name
                _status.value = "attached: ${msg.name}"
                publish()
            }
        }
        is ServerMsg.History -> onHistory(msg)
        is ServerMsg.Discovered -> {
            _discovered.value = msg.sessions
            _discoverError.value = ""
            // Re-derive the attached title from the fresh list by stable id. After a
            // server switch the same session can carry a different name here, leaving the
            // title stale; if the current server calls our attached id something else,
            // migrate the name-keyed state (logs/oldest/hasMore + digests) and title.
            if (_attachedId.value.isNotEmpty()) {
                val cur = msg.sessions.find { it.sessionId == _attachedId.value }?.name
                if (cur != null && cur != _attachedName.value) {
                    _attachedName.value?.let { from ->
                        logs.remove(from)?.let { logs[cur] = it }
                        oldest.remove(from)?.let { oldest[cur] = it }
                        hasMore.remove(from)?.let { hasMore[cur] = it }
                        session.migrate(from, cur) // held + server digests
                        if (currentKey == from) currentKey = cur
                    }
                    _attachedName.value = cur
                    prefs.lastSession = cur
                    _status.value = "attached: $cur"
                    publish()
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
        is ServerMsg.Err -> {
            // Version skew: an older server rejects the connect-time `digest` probe
            // with bad_message — harmless (we fall back to fetching history), so
            // swallow it instead of a scary chat note (mirrors the Android client).
            if (msg.code == "bad_message" && msg.message.contains("digest")) return
            // A failed turn ends it: clear the streamed/spoken turn state so a later
            // stray close for the session isn't misread as "streamed" (Android parity).
            if (msg.code == "turn_failed") { streamedSessions.clear(); spokenReplyCounts.clear() }
            _activity.value = ""
            if (_usageLoading.value) _usageLoading.value = false
            // Turn-terminal errors carry a turn id and can be redelivered buffered
            // on reconnect — drop the repeated row (state above is idempotent).
            if (session.terminalSeen(currentKey, msg.turn)) return
            if (msg.code in setOf("session_active", "not_found", "bad_delete", "bad_adopt", "discover_failed")) {
                _discoverError.value = msg.message
            } else addChat(Role.SYSTEM, "⚠️ ${msg.code}: ${msg.message}")
        }
        is ServerMsg.TurnInterrupted -> {
            _activity.value = ""; streamedSessions.remove(msg.name); spokenReplyCounts.remove(msg.name)
            addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.", key = msg.name)
        }
        is ServerMsg.TurnStopped -> {
            _activity.value = ""; streamedSessions.remove(msg.name); spokenReplyCounts.remove(msg.name)
            // A redelivered stop (buffered terminal, keyed by turn id) — drop the row.
            if (!session.terminalSeen(msg.name, msg.turn)) {
                addChat(Role.SYSTEM, "⏹ stopped that turn.", key = msg.name)
            }
        }
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

internal fun WebAppController.attachUsageToLastClaude(key: String, usage: com.bam.spawner.net.TokenUsage) {
    val log = logs[key] ?: return
    val idx = log.indexOfLast { it.role == Role.CLAUDE }
    if (idx < 0) return
    logs[key] = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
    if (key == currentKey) publish()
}

internal fun WebAppController.onHistory(msg: ServerMsg.History) {
    // `unchanged` answers a top-page freshness check whose have_hash still matched:
    // our in-memory transcript is current, so keep it untouched and just refresh the
    // stored digest so future freshness checks stand.
    if (msg.unchanged) {
        if (msg.hash.isNotEmpty()) session.recordSynced(msg.name, msg.count, msg.hash)
        loadingOlder = false
        return
    }
    val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts) }
    val existing = logs[msg.name] ?: emptyList()
    logs[msg.name] = if (loadingOlder) {
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
    loadingOlder = false
    oldest[msg.name] = hist.firstOrNull()?.index ?: (oldest[msg.name] ?: 0)
    hasMore[msg.name] = msg.more
    // Record the chain digest this page belongs to so a later reattach can
    // short-circuit the fetch when the server hash still matches what we hold.
    if (msg.hash.isNotEmpty()) session.recordSynced(msg.name, msg.count, msg.hash)
    if (msg.name == currentKey) { publish(); _scrollTick.value = _scrollTick.value + 1 }
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

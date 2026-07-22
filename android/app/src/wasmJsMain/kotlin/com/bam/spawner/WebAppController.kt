package com.bam.spawner

import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.AgentInfo
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.HelloConfig
import com.bam.spawner.net.Host
import com.bam.spawner.net.Identity
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ProfileInfo
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.SessionSync
import com.bam.spawner.net.SpawnerClient
import com.bam.spawner.net.UsageReport
import com.bam.spawner.tts.Markdown
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch
import kotlin.io.encoding.Base64
import kotlin.io.encoding.ExperimentalEncodingApi
import kotlin.js.JsAny
import kotlin.js.JsString
import com.bam.spawner.net.Codecs

// Hard cap on one hands-free utterance (matches the phone's Endpointer 15 s cap): a clip
// that never hits the silence gate is shipped anyway rather than growing without bound.
private const val HANDS_FREE_MAX_MS = 15000

/**
 * The browser's [AppController]: it wires the shared [SpawnerClient]'s parsed [ServerMsg]s to
 * state flows and maps the UI's method calls to `Outbound` sends. It replicates the Android
 * `VoiceController`'s message handling in a lighter chat/history model (no watchdog), and drives
 * browser audio itself: push-to-talk, SpeechSynthesis TTS, and VAD-gated hands-free. The audio
 * hooks aren't on the interface (like Android); the web shell calls them directly.
 */
@OptIn(ExperimentalEncodingApi::class)
class WebAppController(private val prefs: Prefs) : AppController {
    private var client: SpawnerClient? = null
    private val scope = CoroutineScope(Dispatchers.Default + SupervisorJob())

    private val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()
    private val _status = MutableStateFlow("disconnected")
    override val status: StateFlow<String> = _status.asStateFlow()

    // Per-session chat logs, keyed by session name; currentKey is the visible one.
    private val logs = mutableMapOf<String, List<ChatMessage>>()
    private val oldest = mutableMapOf<String, Int>()
    private val hasMore = mutableMapOf<String, Boolean>()
    private var currentKey = ""
    private var loadingOlder = false
    private val streamedSessions = mutableSetOf<String>()
    // Per-session count of streamed replies already spoken this turn, for the
    // "speak initial replies" refinement of summary-only mode (mirrors the phone).
    // Reset at every turn boundary alongside [streamedSessions].
    private val spokenReplyCounts = mutableMapOf<String, Int>()

    // Session focus, per-session digest freshness (the phone's cache-validation fast-path,
    // minus the on-disk cache — the browser holds transcripts in memory only), and the one
    // true index-aware chat de-dup are reconciled through one shared commonMain point
    // (sibling to CatalogueSync) so this controller and the Android controller can't drift.
    // It owns the digest caches and the previous-session bookkeeping; this controller keeps
    // the in-memory log storage, StateFlow wiring, and index-sorted history merge (below).
    private val session = SessionSync(object : SessionSync.Host {
        override fun send(frame: String) { client?.send(frame) }
        override fun discovered() = _discovered.value
        override fun attachedId() = _attachedId.value
        override fun attachedName() = _attachedName.value
        override fun attachedAgent() = _attachedAgent.value
        override fun attachedModel() = _attachedModel.value
        override fun heldContent(name: String) = logs[name]?.any { it.index >= 0 } == true
        override fun dropRows(name: String) = dropSessionCache(name)
    })

    // dropSessionCache forgets every name-keyed trace of a session's transcript (in-memory
    // log + paging cursors + held/server digests) so the next history fetch rebuilds it from
    // scratch. Used when a clear/compress OR a same-name session_id rotation wipes the
    // session server-side: the old rows carry stale indexes and merging a fresh page over
    // them would duplicate, so discard wholesale and refetch. Mirror of Android's method.
    private fun dropSessionCache(name: String) {
        logs.remove(name)
        hasMore.remove(name)
        oldest.remove(name)
        session.drop(name) // held + server digests
        if (name == currentKey) publish()
    }

    private val _chat = MutableStateFlow<List<ChatMessage>>(emptyList())
    override val chat: StateFlow<List<ChatMessage>> = _chat.asStateFlow()
    private val _hasMoreHistory = MutableStateFlow(false)
    override val hasMoreHistory: StateFlow<Boolean> = _hasMoreHistory.asStateFlow()
    private val _scrollTick = MutableStateFlow(0)
    override val scrollTick: StateFlow<Int> = _scrollTick.asStateFlow()
    private val _pending = MutableStateFlow("")
    override val pending: StateFlow<String> = _pending.asStateFlow()

    private val _attachedName = MutableStateFlow<String?>(null)
    override val attachedName: StateFlow<String?> = _attachedName.asStateFlow()
    private val _attachedId = MutableStateFlow("")
    override val attachedId: StateFlow<String> = _attachedId.asStateFlow()
    private val _attachedAgent = MutableStateFlow("")
    override val attachedAgent: StateFlow<String> = _attachedAgent.asStateFlow()
    private val _attachedModel = MutableStateFlow("")
    override val attachedModel: StateFlow<String> = _attachedModel.asStateFlow()
    private val _discovered = MutableStateFlow<List<DiscoveredInfo>>(emptyList())
    override val discovered: StateFlow<List<DiscoveredInfo>> = _discovered.asStateFlow()
    private val _discoverError = MutableStateFlow("")
    override val discoverError: StateFlow<String> = _discoverError.asStateFlow()

    /** Locally bump a session's sidebar metadata (recency + busy cue) the instant a
     *  message arrives, so the list re-sorts and shows "working…" without waiting for
     *  the next `discover` round trip. A no-op if the session isn't in the list yet
     *  (a later discover fills it in). Never persisted — the authoritative snapshot
     *  still comes from the server's `discovered` frame. */
    private fun touchDiscovered(name: String, busy: Boolean? = null) {
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

    // Voice pill: OFF until hands-free is on, then LISTENING/CAPTURING/SPEAKING driven by
    // the VAD + TTS (see the hands-free section); push-to-talk leaves it OFF.
    private val _voiceState = MutableStateFlow(VoiceState.OFF)
    override val voiceState: StateFlow<VoiceState> = _voiceState.asStateFlow()
    private val _speaking = MutableStateFlow(false)
    override val speaking: StateFlow<Boolean> = _speaking.asStateFlow()

    // Browser audio output. The page speaks via SpeechSynthesis to the OS default sink, so the
    // only meaningful routing is Speaker (voice on) vs Mute (voice off). Any saved earpiece/
    // bluetooth value (from the phone's registry) normalizes to Speaker. Persisted like the phone.
    private val _audioOutput = MutableStateFlow(
        if (runCatching { AudioOutput.valueOf(prefs.audioOutput.uppercase()) }.getOrNull() == AudioOutput.MUTE)
            AudioOutput.MUTE
        else
            AudioOutput.SPEAKER,
    )
    val audioOutput: StateFlow<AudioOutput> = _audioOutput.asStateFlow()

    /** Switch the browser voice on (Speaker) or off (Mute); Mute also halts any current utterance. */
    fun setAudioOutput(out: AudioOutput) {
        val o = if (out == AudioOutput.MUTE) AudioOutput.MUTE else AudioOutput.SPEAKER
        _audioOutput.value = o
        prefs.audioOutput = o.name.lowercase()
        if (o == AudioOutput.MUTE) {
            cancelServerSpeech()
            cancelSpeech()
            _speaking.value = false
        }
    }
    private val _activity = MutableStateFlow("")
    override val activity: StateFlow<String> = _activity.asStateFlow()

    // Push-to-talk mic status text for the shared input bar ("listening…" while capturing).
    private val _micText = MutableStateFlow("")
    val micText: StateFlow<String> = _micText.asStateFlow()
    private var capturing = false
    private var speakWatch: Job? = null

    private val _lastTurnUsage = MutableStateFlow<TurnUsageInfo?>(null)
    override val lastTurnUsage: StateFlow<TurnUsageInfo?> = _lastTurnUsage.asStateFlow()
    private val _rateLimit = MutableStateFlow<RateLimitInfo?>(null)
    override val rateLimit: StateFlow<RateLimitInfo?> = _rateLimit.asStateFlow()
    private val _usageReport = MutableStateFlow<UsageReport?>(null)
    override val usageReport: StateFlow<UsageReport?> = _usageReport.asStateFlow()
    private val _usageLoading = MutableStateFlow(false)
    override val usageLoading: StateFlow<Boolean> = _usageLoading.asStateFlow()

    private val _whisperModel = MutableStateFlow(prefs.whisperModel)
    override val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()
    // The fast (draft/detection, "quick") server's model; "" = none configured.
    private val _whisperFastModel = MutableStateFlow(prefs.whisperFastModel)
    override val whisperFastModel: StateFlow<String> = _whisperFastModel.asStateFlow()
    // Catalogue offered by the picker; not persisted — re-sent on connect.
    private val _whisperModels = MutableStateFlow<List<String>>(emptyList())
    override val whisperModels: StateFlow<List<String>> = _whisperModels.asStateFlow()
    // Which catalogue models are already downloaded on the server.
    private val _whisperModelsLocal = MutableStateFlow<List<String>>(emptyList())
    override val whisperModelsLocal: StateFlow<List<String>> = _whisperModelsLocal.asStateFlow()
    // Live model-download progress; null when no fetch is in flight.
    private val _whisperDownload = MutableStateFlow<WhisperDownloadInfo?>(null)
    override val whisperDownload: StateFlow<WhisperDownloadInfo?> = _whisperDownload.asStateFlow()
    // Whether the server offers Kokoro TTS (hello_ok `tts`) — with the Server
    // voice toggle on, replies are synthesized server-side and streamed back
    // (see speak() below); browser SpeechSynthesis remains the fallback.
    private val _serverTtsAvailable = MutableStateFlow(false)
    override val serverTtsAvailable: StateFlow<Boolean> = _serverTtsAvailable.asStateFlow()
    // Kokoro's voice catalogue + server default (tts_voices reply; feeds the picker).
    private val _ttsVoices = MutableStateFlow<List<String>>(emptyList())
    override val ttsVoices: StateFlow<List<String>> = _ttsVoices.asStateFlow()
    private val _ttsVoiceDefault = MutableStateFlow("")
    override val ttsVoiceDefault: StateFlow<String> = _ttsVoiceDefault.asStateFlow()

    // Server-TTS speak bookkeeping (single-threaded on the JS main loop, so no
    // locking): id -> stripped text of each in-flight speak, kept for the
    // SpeechSynthesis fallback when the server refuses (error-bearing speak_end).
    private var speakSeq = 0L
    private val speakTexts = LinkedHashMap<String, String>()
    private var speakStreamId: String? = null // utterance whose binary frames are arriving
    private var speakStreamLive = false // false = cancelled/foreign: drop its remaining frames
    private val _ask = MutableStateFlow<List<AskQuestion>?>(null)
    override val ask: StateFlow<List<AskQuestion>?> = _ask.asStateFlow()

    // The four app-managed catalogues (hosts, identities, profiles, providers) are
    // reconciled through one shared commonMain point so this controller and the Android
    // controller can't drift; it owns the StateFlows the UI reads and the outbound
    // mutators. The server persists each and re-broadcasts its list message.
    private val catalogues = com.bam.spawner.net.CatalogueSync { client?.send(it) }
    override val agents: StateFlow<List<AgentInfo>> = catalogues.agents
    override val profiles: StateFlow<List<ProfileInfo>> = catalogues.profiles
    override val spokenTokens: StateFlow<List<com.bam.spawner.net.SpokenTokenInfo>> = catalogues.spokenTokens
    private val _spokenActions = MutableStateFlow<List<com.bam.spawner.net.ActionInfo>>(emptyList())
    override val spokenActions: StateFlow<List<com.bam.spawner.net.ActionInfo>> = _spokenActions.asStateFlow()
    override fun putSpokenToken(t: com.bam.spawner.net.SpokenTokenInfo) = catalogues.putSpokenToken(t)
    override fun deleteSpokenToken(name: String) = catalogues.deleteSpokenToken(name)

    private val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    override val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()
    private val _fileSaved = MutableSharedFlow<String>(extraBufferCapacity = 4)
    override val fileSaved: SharedFlow<String> = _fileSaved.asSharedFlow()
    private val _fileData = MutableSharedFlow<ServerMsg.FileData>(extraBufferCapacity = 4)
    override val fileData: SharedFlow<ServerMsg.FileData> = _fileData.asSharedFlow()

    override val hosts: StateFlow<List<Host>> = catalogues.hosts
    override val identities: StateFlow<List<Identity>> = catalogues.identities

    /** (Re)connect to [url] with [token], sending the hello handshake built from prefs. */
    fun connect(url: String, token: String) {
        client?.close()
        _status.value = "connecting…"
        val hello = HelloConfig(
            prefs.endToken, prefs.wakeToken, prefs.speakToken, prefs.dictationGate,
            prefs.wakeService,
            prefs.sttMode, prefs.sttModel, prefs.aliasMap(),
            prefs.brief, prefs.interactive,
            prefs.warmCompress, prefs.autoCompress, prefs.autoCompressThreshold,
        )
        client = SpawnerClient(
            url = url, token = token, clientId = prefs.clientId, hello = hello,
            onMessage = ::onMessage,
            onConnected = { up ->
                _connected.value = up
                if (!up) {
                    _status.value = "reconnecting…"
                    cancelServerSpeech() // a dropped socket orphans any in-flight speak streams
                }
            },
            onAudio = ::onSpeakFrame,
            catalogueDigests = { catalogues.digests() },
        ).also { it.connect() }
    }

    private fun publish() {
        _chat.value = logs[currentKey] ?: emptyList()
        _hasMoreHistory.value = hasMore[currentKey] ?: false
    }

    private fun focusKnownSession(target: DiscoveredInfo, syncServer: Boolean) {
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

    private fun addChat(role: Role, text: String, usage: com.bam.spawner.net.TokenUsage? = null, key: String = currentKey) {
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

    private fun roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE

    private fun onMessage(msg: ServerMsg) {
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
    private fun onReadLast(count: Int) {
        val claude = _chat.value.filter { it.role == Role.CLAUDE }.takeLast(count.coerceAtLeast(1))
        if (claude.isEmpty()) speak("nothing to read yet")
        else speak(claude.joinToString(". … ") { it.text })
        _scrollTick.value = _scrollTick.value + 1
    }

    private fun attachUsageToLastClaude(key: String, usage: com.bam.spawner.net.TokenUsage) {
        val log = logs[key] ?: return
        val idx = log.indexOfLast { it.role == Role.CLAUDE }
        if (idx < 0) return
        logs[key] = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
        if (key == currentKey) publish()
    }

    private fun onHistory(msg: ServerMsg.History) {
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

    // --- AppController methods -> Outbound sends ----------------------------
    override fun sendText(text: String) {
        val t = text.trim()
        if (t.isEmpty()) return
        addChat(Role.USER, t)
        client?.send(Outbound.utterance(t, sessionId = _attachedId.value))
    }
    override fun focusSession(session: DiscoveredInfo) = focusKnownSession(session, syncServer = true)
    override fun attachTo(name: String) {
        _discovered.value.firstOrNull { it.registered && it.name == name }?.let {
            focusKnownSession(it, syncServer = true)
            return
        }
        client?.send(Outbound.attach(name))
    }
    override fun detach() {
        session.rememberPrevious()
        _activity.value = ""
        _pending.value = ""
        _lastTurnUsage.value = null
        _attachedId.value = ""
        _attachedName.value = null
        _attachedAgent.value = ""
        _attachedModel.value = ""
        prefs.lastSession = ""
        prefs.lastSessionId = ""
        _status.value = "connected"
        currentKey = ""
        publish()
        client?.send(Outbound.detach())
    }
    override fun swap() {
        when (val t = session.swapTarget()) {
            is SessionSync.SwapTarget.Server -> client?.send(Outbound.swap())
            is SessionSync.SwapTarget.Gone -> _status.value = "previous session is gone"
            is SessionSync.SwapTarget.Focus -> focusKnownSession(t.session, syncServer = true)
        }
    }
    override fun abortTurn() { client?.send(Outbound.abort()) }
    override fun loadOlder() {
        val before = oldest[currentKey] ?: return
        if (before <= 0 || loadingOlder) return
        loadingOlder = true
        client?.send(Outbound.history(currentKey, before))
    }
    override fun submitAnswers(text: String) { _ask.value = null; sendText(text) }
    override fun dismissAsk() { _ask.value = null }

    override fun discover() { client?.send(Outbound.discover()) }
    override fun adopt(sessionId: String, dir: String) { client?.send(Outbound.adopt(sessionId, dir)) }
    override fun deleteDiscovered(sessionId: String) { client?.send(Outbound.deleteDiscovered(sessionId)) }
    override fun renameDiscovered(sessionId: String, dir: String, newName: String) {
        client?.send(Outbound.renameDiscovered(sessionId, dir, newName))
    }
    override fun setAgent(sessionId: String, dir: String, agent: String, model: String) {
        client?.send(Outbound.setAgent(sessionId, dir, agent, model))
    }
    override fun spawnAt(path: String, target: String, host: String, agent: String, model: String, profile: String) { client?.send(Outbound.spawnAt(path, target = target, host = host, agent = agent, model = model, profile = profile)) }
    override fun spawnNewFolder(parent: String, name: String, target: String, host: String, agent: String, model: String, profile: String) {
        val clean = name.trim().trim('/')
        if (clean.isEmpty()) return
        client?.send(Outbound.spawnAt("$parent/$clean", create = true, target = target, host = host, agent = agent, model = model, profile = profile))
    }

    override fun browse(path: String, host: String, files: Boolean) { client?.send(Outbound.browse(path, host, files)) }
    override fun uploadFile(dir: String, name: String, contentB64: String, host: String) { client?.send(Outbound.upload(dir, name, contentB64, host)) }
    override fun downloadFile(path: String, host: String) { client?.send(Outbound.download(path, host)) }
    override fun attachedDirHost(): Pair<String, String>? =
        _discovered.value.firstOrNull { it.sessionId == _attachedId.value }?.let { it.dir to it.host }

    // The four app-managed catalogues' mutators delegate to the shared reconciler
    // (see CatalogueSync); the server broadcasts the refreshed list after each change.
    override fun requestHosts() = catalogues.requestHosts()
    override fun putHost(host: Host) = catalogues.putHost(host)
    override fun deleteHost(name: String) = catalogues.deleteHost(name)
    override fun requestIdentities() = catalogues.requestIdentities()
    override fun createIdentity(name: String, user: String, password: String, genKey: Boolean) =
        catalogues.createIdentity(name, user, password, genKey)
    override fun importIdentity(name: String, user: String, password: String, keyPath: String) =
        catalogues.importIdentity(name, user, password, keyPath)
    override fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) =
        catalogues.updateIdentity(name, user, setPassword, password)
    override fun deleteIdentity(name: String) = catalogues.deleteIdentity(name)
    override fun putProfile(p: ProfileInfo) = catalogues.putProfile(p)
    override fun deleteProfile(name: String) = catalogues.deleteProfile(name)
    override fun setDefaultProfile(name: String) = catalogues.setDefaultProfile(name)
    override fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>) =
        catalogues.putProvider(agent, defaultModel, voiceModels)

    override fun requestUsage() { _usageLoading.value = true; _usageReport.value = null; client?.send(Outbound.usage()) }
    override fun dismissUsage() { _usageLoading.value = false; _usageReport.value = null }

    // Whisper model keeps its dedicated message: set_whisper_model is what triggers the
    // resident-server load/download. The server then persists the result into the synced
    // settings catalogue and broadcasts `settings`, so the choice still syncs across clients.
    override fun setWhisperModel(model: String, fast: Boolean) { client?.send(Outbound.setWhisperModel(model, fast)) }
    // Auto-compress + summary-only are synced settings: each scalar is its own keyed record,
    // routed through the shared catalogue mutator (last-writer-wins), not a device-local write.
    override fun setAutoCompress(warm: Boolean, auto: Boolean, thresholdK: Int) {
        catalogues.putSetting("warm_compress", warm.toString())
        catalogues.putSetting("auto_compress", auto.toString())
        catalogues.putSetting("auto_compress_threshold", thresholdK.toString())
    }
    override fun setSummaryOnly(on: Boolean) {
        prefs.summaryOnlySpeech = on
        catalogues.putSetting("summary_only", on.toString())
    }
    override fun restartServer(mode: String) { client?.send(Outbound.restart(mode)) }

    // mirrorSettingsToPrefs folds the inbound shared-settings catalogue into the
    // device-local Prefs the settings UI seeds from, so a change synced from another
    // client (or the server) is reflected here. Whisper models drive their own StateFlows
    // via the `whisper_model` broadcast; here we mirror only the config scalars.
    private fun mirrorSettingsToPrefs() {
        catalogues.settingValue("warm_compress")?.let { prefs.warmCompress = it == "true" }
        catalogues.settingValue("auto_compress")?.let { prefs.autoCompress = it == "true" }
        catalogues.settingValue("auto_compress_threshold")?.let { prefs.autoCompressThreshold = it.toIntOrNull() ?: prefs.autoCompressThreshold }
        catalogues.settingValue("summary_only")?.let { prefs.summaryOnlySpeech = it == "true" }
    }

    // --- Push-to-talk mic capture (concrete, off the interface like Android) -------
    // Mirrors the phone: grab the mic on press, send the whole clip on release as
    // `pcm16` (wake → binary audio → audio_end). getUserMedia may prompt on first use;
    // if the button is released before permission resolves, stopMic returns "" and we
    // simply send nothing.

    /** Mic button pressed: barge-in over any speech, then start capturing. */
    fun startTalking() {
        if (capturing) return
        cancelServerSpeech(); cancelSpeech(); _speaking.value = false // barge-in
        capturing = true
        _micText.value = "listening…"
        startMic().then<JsAny?> { res: JsString ->
            val s = res.toString()
            if (s.startsWith("err:") && capturing) {
                capturing = false; _micText.value = ""
                addChat(Role.SYSTEM, "⚠️ mic unavailable (${s.removePrefix("err:")})")
            }
            null
        }
    }

    /** Mic button released: stop, and if we captured anything, ship the clip. */
    fun stopTalking() {
        if (!capturing) return
        capturing = false
        _micText.value = ""
        val b64 = stopMic().toString()
        if (b64.isEmpty()) return
        val pcm = Base64.decode(b64)
        client?.send(Outbound.wake(Codecs.PCM16, sessionId = _attachedId.value))
        client?.sendAudio(pcm)
        client?.send(Outbound.audioEnd())
    }

    /** Swipe-cancel: drop the capture without sending. */
    fun cancelTalking() {
        if (!capturing) return
        capturing = false
        _micText.value = ""
        cancelMic()
    }

    /** Stop-speaking button / "stop" barge-in: halt TTS now. */
    fun stopSpeaking() { cancelServerSpeech(); cancelSpeech(); _speaking.value = false }

    // --- Hands-free (VAD-gated always-listening) --------------------------------
    // The browser analogue of the phone: one open mic, a JS-side energy VAD that
    // segments each utterance, and one clip per utterance shipped the same way a
    // push-to-talk clip is — but with hands_free=true, so the server streaming-appends
    // until the end token commits. A poll loop drains finished clips and tracks the
    // LISTENING/CAPTURING/SPEAKING pill. VAD dials + the mic toggle persist in prefs.
    private var handsFreeJob: Job? = null

    /** Toggle always-listening on: open the mic under the shared VAD dials, then loop. */
    fun startHandsFree() {
        if (handsFreeJob != null) return
        startHandsFreeMic(prefs.vadThreshold, prefs.vadOnsetMs, prefs.vadSilenceMs, HANDS_FREE_MAX_MS)
            .then<JsAny?> { res: JsString ->
                val s = res.toString()
                if (s.startsWith("err:")) {
                    _voiceState.value = VoiceState.OFF
                    addChat(Role.SYSTEM, "⚠️ mic unavailable (${s.removePrefix("err:")})")
                } else {
                    _voiceState.value = VoiceState.LISTENING
                    handsFreeJob = scope.launch {
                        while (true) {
                            // Reflect what the mic is doing; SPEAKING (our own TTS) wins so the
                            // pill doesn't flicker to CAPTURING on echo the VAD didn't fully reject.
                            _voiceState.value = when {
                                speechActive() || serverSpeechActive() -> VoiceState.SPEAKING
                                handsFreeCapturing() -> VoiceState.CAPTURING
                                else -> VoiceState.LISTENING
                            }
                            val clip = pollHandsFreeClip().toString()
                            if (clip.isNotEmpty()) {
                                val pcm = Base64.decode(clip)
                                client?.send(Outbound.wake(Codecs.PCM16, handsFree = true, sessionId = _attachedId.value))
                                client?.sendAudio(pcm)
                                client?.send(Outbound.audioEnd())
                            }
                            delay(120)
                        }
                    }
                }
                null
            }
    }

    /** Toggle always-listening off: stop the loop and tear the mic down. */
    fun stopHandsFree() {
        handsFreeJob?.cancel(); handsFreeJob = null
        stopHandsFreeMic()
        _voiceState.value = VoiceState.OFF
    }

    // Speak a reply (markdown stripped, same as the phone): with the server's Kokoro
    // voice when the toggle is on and the server offers TTS (the audio streams back
    // and plays via Web Audio), else the browser's SpeechSynthesis. A lightweight
    // poll flips `speaking` off once every engine and in-flight request drains, so
    // the SpeakingBar and its stop button track real playback either way.
    private fun speak(text: String) = speak(text, prefs.ttsVoice)

    private fun speak(text: String, voice: String) {
        if (_audioOutput.value == AudioOutput.MUTE) return
        val spoken = Markdown.toSpeech(text)
        if (spoken.isBlank()) return
        if (prefs.serverTts && _serverTtsAvailable.value && _connected.value) {
            val id = "s${++speakSeq}"
            speakTexts[id] = spoken
            // Runaway guard; the server refuses past 32 queued anyway.
            while (speakTexts.size > 64) speakTexts.remove(speakTexts.keys.first())
            client?.send(Outbound.speak(id, spoken, voice = voice, format = "mp3"))
        } else {
            speakText(spoken)
        }
        _speaking.value = true
        if (speakWatch?.isActive != true) {
            speakWatch = scope.launch {
                while (speakTexts.isNotEmpty() || speechActive() || serverSpeechActive()) delay(250)
                _speaking.value = false
            }
        }
    }

    /** Voice-picker preview: speak a short sample in [voice] through the server. */
    override fun previewTtsVoice(voice: String) {
        speak("Hi bud — this is how I'd sound.", voice)
    }

    /** speak_audio: the next binary frames are this utterance's audio. Anything we
     *  didn't ask for (or a codec we can't decode) is dropped and falls back on
     *  its speak_end. */
    private fun onSpeakAudio(msg: ServerMsg.SpeakAudio) {
        speakStreamId = msg.id
        speakStreamLive = msg.codec == "mp3" && speakTexts.containsKey(msg.id)
        if (speakStreamLive) serverSpeakBegin()
    }

    /** A server→client binary frame — always speak audio (the only binary the
     *  server sends; ordered on the same socket as its speak_audio header). */
    private fun onSpeakFrame(data: ByteArray) {
        if (speakStreamLive) serverSpeakChunk(Base64.encode(data))
    }

    private fun onSpeakEnd(msg: ServerMsg.SpeakEnd) {
        val wasLive = speakStreamLive && speakStreamId == msg.id
        if (speakStreamId == msg.id) {
            speakStreamId = null
            speakStreamLive = false
        }
        val text = speakTexts.remove(msg.id)
        if (wasLive) serverSpeakEnd() // decode the clip and queue it for playback
        // Refused (tts disabled / queue full / synthesis failed) → browser voice.
        // A stream that died part-way (wasLive) already queued partial audio;
        // don't also replay the whole utterance over it.
        if (msg.error.isNotEmpty() && text != null && !wasLive) speakText(text)
    }

    /** Forget all in-flight server speaks and silence their playback (barge-in,
     *  mute, disconnect). Frames still arriving for a cancelled utterance are
     *  dropped until its speak_end passes. */
    private fun cancelServerSpeech() {
        // speak_stop tells the server to drop its queue and abort the in-flight
        // synthesis too (moot when disconnected — the outbox just drops it).
        if (speakTexts.isNotEmpty() || speakStreamLive) client?.send(Outbound.speakStop())
        speakTexts.clear()
        speakStreamLive = false
        cancelServerSpeechPlayback()
    }
}

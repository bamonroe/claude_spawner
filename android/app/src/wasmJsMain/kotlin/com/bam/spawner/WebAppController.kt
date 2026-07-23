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
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.flow.MutableSharedFlow
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asSharedFlow
import kotlinx.coroutines.flow.asStateFlow

// Hard cap on one hands-free utterance (matches the phone's Endpointer 15 s cap): a clip
// that never hits the silence gate is shipped anyway rather than growing without bound.
// Referenced by WebAppControllerAudio.kt's startHandsFree.
internal const val HANDS_FREE_MAX_MS = 15000

/**
 * The browser's [AppController]: it wires the shared [SpawnerClient]'s parsed [ServerMsg]s to
 * state flows and maps the UI's method calls to `Outbound` sends. It replicates the Android
 * `VoiceController`'s message handling in a lighter chat/history model (no watchdog), and drives
 * browser audio itself: push-to-talk, SpeechSynthesis TTS, and VAD-gated hands-free. The audio
 * hooks aren't on the interface (like Android); the web shell calls them directly.
 */
class WebAppController(internal val prefs: Prefs) : AppController {
    internal var client: SpawnerClient? = null
    internal val scope = CoroutineScope(Dispatchers.Default + SupervisorJob())

    internal val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()
    internal val _status = MutableStateFlow("disconnected")
    override val status: StateFlow<String> = _status.asStateFlow()

    // Per-session chat logs, keyed by session name; currentKey is the visible one.
    internal val logs = mutableMapOf<String, List<ChatMessage>>()
    internal val oldest = mutableMapOf<String, Int>()
    internal val hasMore = mutableMapOf<String, Boolean>()
    internal var currentKey = ""
    internal var loadingOlder = false
    internal val streamedSessions = mutableSetOf<String>()
    // Per-session count of streamed replies already spoken this turn, for the
    // "speak initial replies" refinement of summary-only mode (mirrors the phone).
    // Reset at every turn boundary alongside [streamedSessions].
    internal val spokenReplyCounts = mutableMapOf<String, Int>()

    // Session focus, per-session digest freshness (the phone's cache-validation fast-path,
    // minus the on-disk cache — the browser holds transcripts in memory only), and the one
    // true index-aware chat de-dup are reconciled through one shared commonMain point
    // (sibling to CatalogueSync) so this controller and the Android controller can't drift.
    // It owns the digest caches and the previous-session bookkeeping; this controller keeps
    // the in-memory log storage, StateFlow wiring, and index-sorted history merge (below).
    internal val session = SessionSync(object : SessionSync.Host {
        override fun send(frame: String) { client?.send(frame) }
        override fun discovered() = _discovered.value
        override fun attachedId() = _attachedId.value
        override fun attachedName() = _attachedName.value
        override fun attachedAgent() = _attachedAgent.value
        override fun attachedModel() = _attachedModel.value
        override fun heldContent(name: String) = logs[name]?.any { it.index >= 0 } == true
        override fun dropRows(name: String) = dropSessionCache(name)
    })

    internal val _chat = MutableStateFlow<List<ChatMessage>>(emptyList())
    override val chat: StateFlow<List<ChatMessage>> = _chat.asStateFlow()
    internal val _hasMoreHistory = MutableStateFlow(false)
    override val hasMoreHistory: StateFlow<Boolean> = _hasMoreHistory.asStateFlow()
    internal val _scrollTick = MutableStateFlow(0)
    override val scrollTick: StateFlow<Int> = _scrollTick.asStateFlow()
    internal val _pending = MutableStateFlow("")
    override val pending: StateFlow<String> = _pending.asStateFlow()

    internal val _attachedName = MutableStateFlow<String?>(null)
    override val attachedName: StateFlow<String?> = _attachedName.asStateFlow()
    internal val _attachedId = MutableStateFlow("")
    override val attachedId: StateFlow<String> = _attachedId.asStateFlow()
    internal val _attachedAgent = MutableStateFlow("")
    override val attachedAgent: StateFlow<String> = _attachedAgent.asStateFlow()
    internal val _attachedModel = MutableStateFlow("")
    override val attachedModel: StateFlow<String> = _attachedModel.asStateFlow()
    internal val _discovered = MutableStateFlow<List<DiscoveredInfo>>(emptyList())
    override val discovered: StateFlow<List<DiscoveredInfo>> = _discovered.asStateFlow()
    internal val _discoverError = MutableStateFlow("")
    override val discoverError: StateFlow<String> = _discoverError.asStateFlow()

    // Voice pill: OFF until hands-free is on, then LISTENING/CAPTURING/SPEAKING driven by
    // the VAD + TTS (see the hands-free section); push-to-talk leaves it OFF.
    internal val _voiceState = MutableStateFlow(VoiceState.OFF)
    override val voiceState: StateFlow<VoiceState> = _voiceState.asStateFlow()
    internal val _speaking = MutableStateFlow(false)
    override val speaking: StateFlow<Boolean> = _speaking.asStateFlow()

    // Browser audio output. The page speaks via SpeechSynthesis to the OS default sink, so the
    // only meaningful routing is Speaker (voice on) vs Mute (voice off). Any saved earpiece/
    // bluetooth value (from the phone's registry) normalizes to Speaker. Persisted like the phone.
    internal val _audioOutput = MutableStateFlow(
        if (runCatching { AudioOutput.valueOf(prefs.audioOutput.uppercase()) }.getOrNull() == AudioOutput.MUTE)
            AudioOutput.MUTE
        else
            AudioOutput.SPEAKER,
    )
    val audioOutput: StateFlow<AudioOutput> = _audioOutput.asStateFlow()

    internal val _activity = MutableStateFlow("")
    override val activity: StateFlow<String> = _activity.asStateFlow()

    // Push-to-talk mic status text for the shared input bar ("listening…" while capturing).
    internal val _micText = MutableStateFlow("")
    val micText: StateFlow<String> = _micText.asStateFlow()
    internal var capturing = false
    internal var speakWatch: Job? = null

    internal val _lastTurnUsage = MutableStateFlow<TurnUsageInfo?>(null)
    override val lastTurnUsage: StateFlow<TurnUsageInfo?> = _lastTurnUsage.asStateFlow()
    internal val _rateLimit = MutableStateFlow<RateLimitInfo?>(null)
    override val rateLimit: StateFlow<RateLimitInfo?> = _rateLimit.asStateFlow()
    internal val _usageReport = MutableStateFlow<UsageReport?>(null)
    override val usageReport: StateFlow<UsageReport?> = _usageReport.asStateFlow()
    internal val _usageLoading = MutableStateFlow(false)
    override val usageLoading: StateFlow<Boolean> = _usageLoading.asStateFlow()

    internal val _whisperModel = MutableStateFlow(prefs.whisperModel)
    override val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()
    // The fast (draft/detection, "quick") server's model; "" = none configured.
    internal val _whisperFastModel = MutableStateFlow(prefs.whisperFastModel)
    override val whisperFastModel: StateFlow<String> = _whisperFastModel.asStateFlow()
    // Catalogue offered by the picker; not persisted — re-sent on connect.
    internal val _whisperModels = MutableStateFlow<List<String>>(emptyList())
    override val whisperModels: StateFlow<List<String>> = _whisperModels.asStateFlow()
    // Which catalogue models are already downloaded on the server.
    internal val _whisperModelsLocal = MutableStateFlow<List<String>>(emptyList())
    override val whisperModelsLocal: StateFlow<List<String>> = _whisperModelsLocal.asStateFlow()
    // Live model-download progress; null when no fetch is in flight.
    internal val _whisperDownload = MutableStateFlow<WhisperDownloadInfo?>(null)
    override val whisperDownload: StateFlow<WhisperDownloadInfo?> = _whisperDownload.asStateFlow()
    // Whether the server offers Kokoro TTS (hello_ok `tts`) — with the Server
    // voice toggle on, replies are synthesized server-side and streamed back
    // (see speak() below); browser SpeechSynthesis remains the fallback.
    internal val _serverTtsAvailable = MutableStateFlow(false)
    override val serverTtsAvailable: StateFlow<Boolean> = _serverTtsAvailable.asStateFlow()
    // Kokoro's voice catalogue + server default (tts_voices reply; feeds the picker).
    internal val _ttsVoices = MutableStateFlow<List<String>>(emptyList())
    override val ttsVoices: StateFlow<List<String>> = _ttsVoices.asStateFlow()
    internal val _ttsVoiceDefault = MutableStateFlow("")
    override val ttsVoiceDefault: StateFlow<String> = _ttsVoiceDefault.asStateFlow()

    // Server-TTS speak bookkeeping (single-threaded on the JS main loop, so no
    // locking): id -> stripped text of each in-flight speak, kept for the
    // SpeechSynthesis fallback when the server refuses (error-bearing speak_end).
    internal var speakSeq = 0L
    internal val speakTexts = LinkedHashMap<String, String>()
    internal var speakStreamId: String? = null // utterance whose binary frames are arriving
    internal var speakStreamLive = false // false = cancelled/foreign: drop its remaining frames
    internal val _ask = MutableStateFlow<List<AskQuestion>?>(null)
    override val ask: StateFlow<List<AskQuestion>?> = _ask.asStateFlow()

    // The four app-managed catalogues (hosts, identities, profiles, providers) are
    // reconciled through one shared commonMain point so this controller and the Android
    // controller can't drift; it owns the StateFlows the UI reads and the outbound
    // mutators. The server persists each and re-broadcasts its list message.
    internal val catalogues = com.bam.spawner.net.CatalogueSync { client?.send(it) }
    override val agents: StateFlow<List<AgentInfo>> = catalogues.agents
    override val profiles: StateFlow<List<ProfileInfo>> = catalogues.profiles
    override val spokenTokens: StateFlow<List<com.bam.spawner.net.SpokenTokenInfo>> = catalogues.spokenTokens
    internal val _spokenActions = MutableStateFlow<List<com.bam.spawner.net.ActionInfo>>(emptyList())
    override val spokenActions: StateFlow<List<com.bam.spawner.net.ActionInfo>> = _spokenActions.asStateFlow()
    override fun putSpokenToken(t: com.bam.spawner.net.SpokenTokenInfo) = catalogues.putSpokenToken(t)
    override fun deleteSpokenToken(name: String) = catalogues.deleteSpokenToken(name)

    internal val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    override val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()
    internal val _fileSaved = MutableSharedFlow<String>(extraBufferCapacity = 4)
    override val fileSaved: SharedFlow<String> = _fileSaved.asSharedFlow()
    internal val _fileData = MutableSharedFlow<ServerMsg.FileData>(extraBufferCapacity = 4)
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
            onMessage = { onMessage(it) },
            onConnected = { up ->
                _connected.value = up
                if (!up) {
                    _status.value = "reconnecting…"
                    cancelServerSpeech() // a dropped socket orphans any in-flight speak streams
                }
            },
            onAudio = { onSpeakFrame(it) },
            catalogueDigests = { catalogues.digests() },
        ).also { it.connect() }
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

    // --- Push-to-talk mic capture (concrete, off the interface like Android) -------
    // Mirrors the phone: grab the mic on press, send the whole clip on release as
    // `pcm16` (wake → binary audio → audio_end). getUserMedia may prompt on first use;
    // if the button is released before permission resolves, stopMic returns "" and we
    // simply send nothing.

    // --- Hands-free (VAD-gated always-listening) --------------------------------
    // The browser analogue of the phone: one open mic, a JS-side energy VAD that
    // segments each utterance, and one clip per utterance shipped the same way a
    // push-to-talk clip is — but with hands_free=true, so the server streaming-appends
    // until the end token commits. A poll loop drains finished clips and tracks the
    // LISTENING/CAPTURING/SPEAKING pill. VAD dials + the mic toggle persist in prefs.
    internal var handsFreeJob: Job? = null

    /** Voice-picker preview: speak a short sample in [voice] through the server. */
    override fun previewTtsVoice(voice: String) {
        speak("Hi bud — this is how I'd sound.", voice)
    }

}

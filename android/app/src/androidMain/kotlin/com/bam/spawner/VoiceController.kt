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

/** End-token calibration progress: how reliably the detection model hears the token. */
data class CalibrationState(
    val active: Boolean = false,
    val done: Boolean = false,
    val token: String = "",
    val rounds: Int = 10,
    val samples: List<String> = emptyList(), // what was heard each attempt
    val hits: Int = 0,
)

/** The most recent completed turn's token usage, stamped with when it finished
 *  (SystemClock.elapsedRealtime ms) so the UI can count down the ~5-min warm
 *  prompt-cache window from that moment. */

/**
 * Orchestrates the app's voice/chat loop: connects (with auto-reconnect),
 * streams push-to-talk audio, forwards typed utterances, keeps the session list
 * and per-session chat transcript, and reflects server messages into UI state +
 * text-to-speech.
 *
 * Cohesive method clusters live in sibling files as `internal` extension functions
 * on this class (identical to members): VoiceControllerMessages.kt (server-message
 * handling + chat/history paging), VoiceControllerAudio.kt (mic/hands-free/PTT/
 * calibration), VoiceControllerSpeech.kt (TTS playback). State they touch is
 * `internal` below.
 */
class VoiceController(context: Context, internal val settings: SettingsStore) : AppController {
    internal val app = context.applicationContext
    internal val speaker = Speaker(app)
    internal val recorder = OpusRecorder(app)
    internal val notifier = Notifier(app)
    internal var client: SpawnerClient? = null

    /** True while the app UI is in the foreground; drives whether a finished turn
     *  posts a notification. Set by the Activity's lifecycle. */
    @Volatile var appForeground = false

    internal val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()

    internal val _status = MutableStateFlow("disconnected")
    override val status: StateFlow<String> = _status.asStateFlow()

    // Per-session chat logs, keyed by session name ("" = the general/unattached
    // view for dialog + system messages). `_chat` mirrors the current key's log.
    internal val logs = mutableMapOf<String, List<ChatMessage>>()
    internal val oldestIndex = mutableMapOf<String, Int>() // lowest history index held, per session
    internal val hasMore = mutableMapOf<String, Boolean>() // older history remains to page, per session
    internal val loadingOlder = mutableSetOf<String>()      // a page request is in flight, per session
    internal val bridgeTo = mutableMapOf<String, Int>()      // reconnect gap-fill: page older until this index is reached
    internal var currentKey = ""

    // Offline transcript cache. `loadedFromCache` tracks which sessions we've pulled from
    // disk into memory. The digest caches (which the on-disk cache is validated against)
    // and the previous-session bookkeeping live in the shared reconciler below.
    internal val cache = TranscriptCache(File(app.filesDir, "transcripts"))
    internal val discoveredCache = DiscoveredCache(File(app.filesDir, "discovered.json"))
    internal val loadedFromCache = mutableSetOf<String>()

    // Session focus, per-session digest freshness, and the one true index-aware chat de-dup
    // are reconciled through one shared commonMain point (sibling to CatalogueSync) so this
    // controller and the web controller can't drift. It owns the digest caches and the
    // previous-session bookkeeping; this controller keeps the on-disk transcript cache, the
    // StateFlow/settings wiring, and the timestamp-ordered history merge (all below).
    internal val session = com.bam.spawner.net.SessionSync(object : com.bam.spawner.net.SessionSync.Host {
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

    // Whether the current session has older history to page in (drives the
    // "load older" control), and a tick the UI watches to scroll to the bottom.
    internal val _hasMoreHistory = MutableStateFlow(false)
    override val hasMoreHistory: StateFlow<Boolean> = _hasMoreHistory.asStateFlow()
    internal val _scrollTick = MutableStateFlow(0)
    override val scrollTick: StateFlow<Int> = _scrollTick.asStateFlow()

    // Claude sessions found on disk (from `discover`) that can be adopted.
    // Seeded from the on-disk cache so the sidebar is populated on a fresh launch
    // before (or without) a server connection; refreshed on each connect-time sweep.
    internal val _discovered = MutableStateFlow<List<DiscoveredInfo>>(discoveredCache.load())
    override val discovered: StateFlow<List<DiscoveredInfo>> = _discovered.asStateFlow()

    // Last error from a discover/adopt/delete action, shown on the Discover
    // screen (otherwise it would go to the hidden chat log). "" = none.
    internal val _discoverError = MutableStateFlow("")
    override val discoverError: StateFlow<String> = _discoverError.asStateFlow()

    internal val _attachedName = MutableStateFlow<String?>(null)
    override val attachedName: StateFlow<String?> = _attachedName.asStateFlow()

    // The stable on-disk id of the attached session. Names diverge between servers
    // (the same session is "spawner-2" on one, "spawner-3" on another) and change on
    // rename, so we track the id to keep the title correct: rename matches by id, and
    // a fresh session list re-derives the title from whatever the current server calls
    // this id. Exposed so the sidebar highlights the attached row by id (not name).
    // Empty when detached or when the server didn't send an id (older server).
    internal val _attachedId = MutableStateFlow("")
    override val attachedId: StateFlow<String> = _attachedId.asStateFlow()

    // Backend id + model alias of the attached session (for the status-bar badge).
    internal val _attachedAgent = MutableStateFlow("")
    override val attachedAgent: StateFlow<String> = _attachedAgent.asStateFlow()
    internal val _attachedModel = MutableStateFlow("")
    override val attachedModel: StateFlow<String> = _attachedModel.asStateFlow()

    internal val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    override val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()

    // One-shot file-transfer results (message-box 📎 button): an upload's landed path
    // and a download's bytes. SharedFlow, not StateFlow — each is a fire-once event the
    // UI reacts to (prefill the box / write the download), not retained state.
    internal val _fileSaved = MutableSharedFlow<String>(extraBufferCapacity = 4)
    override val fileSaved: SharedFlow<String> = _fileSaved.asSharedFlow()
    internal val _fileData = MutableSharedFlow<ServerMsg.FileData>(extraBufferCapacity = 4)
    override val fileData: SharedFlow<ServerMsg.FileData> = _fileData.asSharedFlow()

    // The four app-managed catalogues (hosts, identities, profiles, providers) are
    // reconciled through one shared commonMain point so this controller and the web
    // controller can't drift; it owns the StateFlows the UI reads and the outbound
    // mutators. The server persists each and re-broadcasts its list message.
    internal val catalogues = com.bam.spawner.net.CatalogueSync { client?.send(it) }
    override val hosts: StateFlow<List<com.bam.spawner.net.Host>> = catalogues.hosts
    override val identities: StateFlow<List<com.bam.spawner.net.Identity>> = catalogues.identities

    internal val _mic = MutableStateFlow("")
    val mic: StateFlow<String> = _mic.asStateFlow()

    internal val _voiceState = MutableStateFlow(VoiceState.OFF)
    override val voiceState: StateFlow<VoiceState> = _voiceState.asStateFlow()

    // Live hands-free draft: what's captured but not yet committed (via end token).
    internal val _pending = MutableStateFlow("")
    override val pending: StateFlow<String> = _pending.asStateFlow()

    // Live mic RMS level (~0..32768) for the audio meter, fed by the running
    // hands-free recorder or a standalone LevelMeter.
    internal val _micLevel = MutableStateFlow(0.0)
    val micLevel: StateFlow<Double> = _micLevel.asStateFlow()

    // Live "Claude is thinking / editing foo.go" indicator; "" when idle.
    internal val _activity = MutableStateFlow("")
    override val activity: StateFlow<String> = _activity.asStateFlow()

    // Token usage of the last completed turn, driving the per-message badge's
    // source data and the status-bar cache-warm countdown. Null until a turn
    // finishes; reset when attaching elsewhere (a new session = a new cache).
    internal val _lastTurnUsage = MutableStateFlow<TurnUsageInfo?>(null)
    override val lastTurnUsage: StateFlow<TurnUsageInfo?> = _lastTurnUsage.asStateFlow()

    // The Claude plan's session-limit state (which usage window is binding, when it
    // resets), refreshed from each turn's rate_limit_event. Shown at the bottom of
    // the sessions drawer. Null until the first turn reports it.
    internal val _rateLimit = MutableStateFlow<RateLimitInfo?>(null)
    override val rateLimit: StateFlow<RateLimitInfo?> = _rateLimit.asStateFlow()

    // On-demand `/usage` report (session/weekly % used). Loading is set while the
    // server runs /usage; report holds the result. A non-null report opens the
    // usage sheet — including from the "usage" voice command (no prior request).
    internal val _usageReport = MutableStateFlow<UsageReport?>(null)
    override val usageReport: StateFlow<UsageReport?> = _usageReport.asStateFlow()
    internal val _usageLoading = MutableStateFlow(false)
    override val usageLoading: StateFlow<Boolean> = _usageLoading.asStateFlow()

    /** Ask the server for the plan's usage report (runs `/usage`); opens the sheet in a loading state. */
    override fun requestUsage() {
        _usageReport.value = null
        _usageLoading.value = true
        client?.send(Outbound.usage())
    }

    /** Dismiss the usage sheet and clear its state. */
    override fun dismissUsage() {
        _usageReport.value = null
        _usageLoading.value = false
    }

    // True while TTS is speaking (drives the tap-to-stop banner).
    private val _speaking = MutableStateFlow(false)
    override val speaking: StateFlow<Boolean> = _speaking.asStateFlow()

    // The resident whisper model the SERVER currently has (server-global). Read on
    // connect; changed only via setWhisperModel. "" until the server reports it.
    internal val _whisperModel = MutableStateFlow(settings.whisperModel)
    override val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()

    // The fast (draft/detection, "quick") server's model; "" = none configured.
    internal val _whisperFastModel = MutableStateFlow(settings.whisperFastModel)
    override val whisperFastModel: StateFlow<String> = _whisperFastModel.asStateFlow()

    // Catalogue offered by the picker; not persisted — re-sent on connect.
    internal val _whisperModels = MutableStateFlow<List<String>>(emptyList())
    override val whisperModels: StateFlow<List<String>> = _whisperModels.asStateFlow()

    // Which catalogue models are already downloaded on the server.
    internal val _whisperModelsLocal = MutableStateFlow<List<String>>(emptyList())
    override val whisperModelsLocal: StateFlow<List<String>> = _whisperModelsLocal.asStateFlow()

    // Whether the connected server offers Kokoro TTS (hello_ok `tts`).
    internal val _serverTtsAvailable = MutableStateFlow(false)
    override val serverTtsAvailable: StateFlow<Boolean> = _serverTtsAvailable.asStateFlow()

    // Kokoro's voice catalogue + server default (tts_voices reply; feeds the picker).
    internal val _ttsVoices = MutableStateFlow<List<String>>(emptyList())
    override val ttsVoices: StateFlow<List<String>> = _ttsVoices.asStateFlow()
    internal val _ttsVoiceDefault = MutableStateFlow("")
    override val ttsVoiceDefault: StateFlow<String> = _ttsVoiceDefault.asStateFlow()

    // --- Server-TTS (Kokoro) speak bookkeeping --------------------------------
    // Everything the net thread and the UI thread both touch sits under speakLock.
    internal val speakLock = Any()
    internal var speakSeq = 0L
    // id -> stripped text of each in-flight speak, kept for the on-device
    // fallback when the server refuses (error-bearing speak_end).
    internal val speakTexts = LinkedHashMap<String, String>()
    internal var speakStreamId: String? = null // utterance whose binary frames are arriving
    internal var speakStreamLive = false // false = cancelled/foreign: drop its remaining frames

    // Live model-download progress; null when no fetch is in flight.
    internal val _whisperDownload = MutableStateFlow<WhisperDownloadInfo?>(null)
    override val whisperDownload: StateFlow<WhisperDownloadInfo?> = _whisperDownload.asStateFlow()

    // Pending clarification questions (interactive mode); null when none.
    internal val _ask = MutableStateFlow<List<com.bam.spawner.net.AskQuestion>?>(null)
    override val ask: StateFlow<List<com.bam.spawner.net.AskQuestion>?> = _ask.asStateFlow()

    // AI backend registry (`agents`) and execution profiles (`profiles`) — both are
    // app-managed catalogues, reconciled through the shared `catalogues` above.
    override val agents: StateFlow<List<com.bam.spawner.net.AgentInfo>> = catalogues.agents
    override val profiles: StateFlow<List<ProfileInfo>> = catalogues.profiles

    // Spoken-token catalogue (`spoken_tokens`) + the closed action set the server
    // advertises (`actions`). The tokens reconcile through `catalogues`; the actions
    // are advertise-only, cached here to populate the editor's action dropdown.
    override val spokenTokens: StateFlow<List<com.bam.spawner.net.SpokenTokenInfo>> = catalogues.spokenTokens
    internal val _spokenActions = MutableStateFlow<List<com.bam.spawner.net.ActionInfo>>(emptyList())
    override val spokenActions: StateFlow<List<com.bam.spawner.net.ActionInfo>> = _spokenActions.asStateFlow()
    override fun putSpokenToken(t: com.bam.spawner.net.SpokenTokenInfo) = catalogues.putSpokenToken(t)
    override fun deleteSpokenToken(name: String) = catalogues.deleteSpokenToken(name)

    // Spoken-audio output routing (earpiece/speaker/bluetooth). `audioOutputs` is
    // what's currently selectable (bluetooth only when a headset is connected);
    // `audioOutput` is the active one.
    internal val audioRouter = AudioRouter(app)
    internal val _audioOutputs = MutableStateFlow(listOf(AudioOutput.EARPIECE, AudioOutput.SPEAKER))
    val audioOutputs: StateFlow<List<AudioOutput>> = _audioOutputs.asStateFlow()
    internal val _audioOutput = MutableStateFlow(AudioOutput.EARPIECE)
    val audioOutput: StateFlow<AudioOutput> = _audioOutput.asStateFlow()
    internal val audioOutputRequest = AtomicInteger(0)
    // Capture (mic) source, picked independently of the output: `audioInputs` is
    // what's currently selectable (headset mic only when a Bluetooth headset is
    // paired); `audioInput` is the active one.
    internal val _audioInputs = MutableStateFlow(listOf(AudioInput.DEVICE))
    val audioInputs: StateFlow<List<AudioInput>> = _audioInputs.asStateFlow()
    internal val _audioInput = MutableStateFlow(AudioInput.DEVICE)
    val audioInput: StateFlow<AudioInput> = _audioInput.asStateFlow()

    internal val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    internal var meter: LevelMeter? = null
    internal var commitTimer: Job? = null

    // A dictation turn is "in flight" from the server's first activity breadcrumb
    // until its reply/error. If the connection drops mid-turn and nothing resolves
    // it within the grace window, we warn the user rather than spin forever — a
    // turn that dies with a server restart/crash otherwise gives no notice.
    @Volatile internal var turnInFlight = false
    internal var lostTurnWatchdog: Job? = null
    private val lostTurnGraceMs = 45_000L
    // Sessions that have streamed at least one live prose chunk for their current
    // turn. Keyed by session name so a gesture swap cannot make a late frame from
    // the previous session render into the newly visible log.
    internal val streamedSessions = mutableSetOf<String>()
    // Per-session count of streamed replies already SPOKEN this turn, for the
    // "speak initial replies" refinement of summary-only mode. Reset at every
    // turn boundary alongside [streamedSessions] so it starts at 0 each turn.
    internal val spokenReplyCounts = mutableMapOf<String, Int>()

    init {
        // While Claude's reply is spoken, raise the recorder's VAD bar (echo) and
        // reflect SPEAKING; when it finishes, return to LISTENING.
        speaker.onSpeakingChanged { speaking ->
            _speaking.value = speaking
            handsFree?.playbackActive = speaking
            if (hfOn) _voiceState.value = if (speaking) VoiceState.SPEAKING else VoiceState.LISTENING
        }
        // Media speaker by default; hands-free switches to echo-cancelled comm audio.
        speaker.setCommMode(settings.handsFree)
        // Migrate the retired "bluetooth" output: it meant "use the whole headset,"
        // which is now the headset output + the headset mic input, picked separately.
        if (settings.audioOutput.equals("bluetooth", ignoreCase = true)) {
            settings.audioOutput = "headset"; settings.micSource = "headset"
        }
        // Restore the saved output (falling back to earpiece if it's no longer
        // available, e.g. the Bluetooth headset is off).
        val saved = runCatching { AudioOutput.valueOf(settings.audioOutput.uppercase()) }.getOrNull()
        val available = audioRouter.available()
        _audioOutputs.value = available
        // Restore the saved capture source alongside it.
        _audioInputs.value = audioRouter.availableInputs()
        _audioInput.value = AudioInput.fromPref(settings.micSource)
        // Restore an explicit saved pick; otherwise (first run / saved device gone)
        // prefer headset-media whenever a headset is already connected at launch.
        val target = when {
            saved != null && saved in available -> saved
            AudioOutput.HEADSET in available -> AudioOutput.HEADSET
            else -> AudioOutput.EARPIECE
        }
        applyAudioOutput(target)
        _audioOutput.value = target
        // Follow headphone plug/unplug live: hand off to the background scope since
        // restarting capture joins the worker thread (the callback is on main).
        audioRouter.registerRouteCallback { scope.launch { onAudioRouteChanged() } }
    }

    fun connect(url: String, token: String) {
        client?.close()
        _status.value = "connecting…"
        val hello = com.bam.spawner.net.HelloConfig(
            settings.endToken, settings.wakeToken, settings.speakToken, settings.dictationGate,
            settings.wakeService,
            settings.sttMode, settings.sttModel, settings.aliasMap(),
            settings.brief, settings.interactive,
            settings.warmCompress, settings.autoCompress, settings.autoCompressThreshold,
        )
        // Pick up a CA pushed over adb (hands-off), then trust it for this wss server.
        settings.autoImportPushedCa()
        val caPem = settings.caCertPem.ifBlank { null }
        client = SpawnerClient(url, token, settings.clientId, hello, this::onMessage, this::onConnected, this::onSpeakFrame, caPem,
            catalogueDigests = { catalogues.digests() })
            .also { it.connect() }
    }

    /** Connect only if we don't already have a client (survives Activity recreation). */
    fun connectIfNeeded(url: String, token: String) {
        if (client == null) connect(url, token)
    }

    override fun sendText(text: String) {
        val t = text.trim()
        if (t.isEmpty()) return
        addChat(Role.USER, t)
        scrollToBottom() // typed message → jump to the latest
        client?.send(Outbound.utterance(t, sessionId = _attachedId.value))
    }

    /** Nudge the chat view to scroll to the newest message. */
    internal fun scrollToBottom() { _scrollTick.value = _scrollTick.value + 1 }

    // --- Sidebar actions ---

    /** Ask the server for all Claude sessions on disk (spawner-created or not). */
    override fun discover() = client?.send(Outbound.discover()).let {}

    // The four app-managed catalogues' mutators all delegate to the shared reconciler
    // (see CatalogueSync); the server broadcasts the refreshed list after each change.
    override fun requestHosts() = catalogues.requestHosts()
    override fun putHost(host: com.bam.spawner.net.Host) = catalogues.putHost(host)
    override fun deleteHost(name: String) = catalogues.deleteHost(name)
    override fun requestIdentities() = catalogues.requestIdentities()
    override fun createIdentity(name: String, user: String, password: String, genKey: Boolean) =
        catalogues.createIdentity(name, user, password, genKey)
    override fun importIdentity(name: String, user: String, password: String, keyPath: String) =
        catalogues.importIdentity(name, user, password, keyPath)
    override fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) =
        catalogues.updateIdentity(name, user, setPassword, password)
    override fun deleteIdentity(name: String) = catalogues.deleteIdentity(name)
    override fun putProfile(p: com.bam.spawner.net.ProfileInfo) = catalogues.putProfile(p)
    override fun deleteProfile(name: String) = catalogues.deleteProfile(name)
    override fun setDefaultProfile(name: String) = catalogues.setDefaultProfile(name)
    override fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>) =
        catalogues.putProvider(agent, defaultModel, voiceModels)

    /** Adopt a discovered session into the registry and attach to it. */
    override fun adopt(sessionId: String, dir: String) = client?.send(Outbound.adopt(sessionId, dir)).let {}

    /** Permanently delete a discovered session's transcript from disk. */
    override fun deleteDiscovered(sessionId: String) = client?.send(Outbound.deleteDiscovered(sessionId)).let {}

    /** Give a discovered session a custom name (registers it by dir if needed). */
    override fun renameDiscovered(sessionId: String, dir: String, newName: String) =
        client?.send(Outbound.renameDiscovered(sessionId, dir, newName)).let {}

    /** Switch a session's AI backend + model (registers it by dir if needed). */
    override fun setAgent(sessionId: String, dir: String, agent: String, model: String) =
        client?.send(Outbound.setAgent(sessionId, dir, agent, model)).let {}

    override fun focusSession(session: DiscoveredInfo) = focusKnownSession(session, syncServer = true)

    override fun attachTo(name: String) {
        _discovered.value.firstOrNull { it.registered && it.name == name }?.let {
            focusKnownSession(it, syncServer = true)
            return
        }
        showLog(name) // switch to that session's log immediately (cached if we have it)
        client?.send(Outbound.attach(name))
    }

    /** Load the previous page of older history for the current session. */
    override fun loadOlder() {
        if (currentKey.isNotEmpty()) fetchOlder(currentKey)
    }

    override fun detach() {
        session.rememberPrevious()
        clearTurnInFlight()
        _activity.value = ""
        _pending.value = ""
        _lastTurnUsage.value = null
        _attachedId.value = ""
        _attachedName.value = null
        _attachedAgent.value = ""
        _attachedModel.value = ""
        settings.lastSession = ""
        settings.lastSessionId = ""
        _status.value = "connected"
        showLog("")
        client?.send(Outbound.detach())
    }

    override fun swap() {
        when (val t = session.swapTarget()) {
            is com.bam.spawner.net.SessionSync.SwapTarget.Server -> client?.send(Outbound.swap()).let {}
            is com.bam.spawner.net.SessionSync.SwapTarget.Gone -> _status.value = "previous session is gone"
            is com.bam.spawner.net.SessionSync.SwapTarget.Focus -> focusKnownSession(t.session, syncServer = true)
        }
    }

    /** Abort the running turn on the attached session (kills the claude child). */
    override fun abortTurn() = client?.send(Outbound.abort()).let {}

    /** Submit answers to the pending interactive questions (from the dialog). The
     *  formatted text goes back as an ordinary turn — Claude has the questions in
     *  context via --resume. */
    override fun submitAnswers(text: String) {
        _ask.value = null
        sendText(text)
    }

    /** Dismiss the questions without answering (they stay in the transcript). */
    override fun dismissAsk() { _ask.value = null }

    internal fun spokenQuestions(qs: List<AskQuestion>): String {
        fun opts(q: AskQuestion) = if (q.options.isEmpty()) "" else " Options: " + q.options.joinToString(", ") + "."
        if (qs.size == 1) return qs[0].q + opts(qs[0])
        return "I have ${qs.size} questions. " +
            qs.mapIndexed { i, q -> "${i + 1}: ${q.q}${opts(q)}" }.joinToString(" ")
    }

    // --- Visual directory browser (New session) ---
    // Browsing is host-scoped: the listing is produced on `host` (its filesystem
    // starting at "/"), so the picker reflects the machine the session will run on.
    override fun browse(path: String, host: String, files: Boolean) =
        client?.send(Outbound.browse(path, host, files)).let {}

    // --- File transfer (message-box 📎 button) ---
    // upload writes a base64 file to <dir>/<name> on host; download reads a file and
    // returns its bytes as `file_data`. host "" = the local/loopback machine.
    override fun uploadFile(dir: String, name: String, contentB64: String, host: String) =
        client?.send(Outbound.upload(dir, name, contentB64, host)).let {}

    override fun downloadFile(path: String, host: String) = client?.send(Outbound.download(path, host)).let {}

    /** The attached session's working dir + host, for the transfer picker's starting
     *  point — looked up from discovery by session_id. Null when nothing is attached
     *  or discovery hasn't surfaced it yet (caller falls back to the host root). */
    override fun attachedDirHost(): Pair<String, String>? {
        val id = _attachedId.value
        if (id.isEmpty()) return null
        val d = _discovered.value.find { it.sessionId == id } ?: return null
        return d.dir to d.host
    }

    override fun spawnAt(path: String, target: String, host: String, agent: String, model: String, profile: String) {
        client?.send(Outbound.spawnAt(path, target = target, host = host, agent = agent, model = model, profile = profile)) // the resulting `attached` switches the view
    }

    /** Create a new project folder `name` under `parent` and spawn a session in it. */
    override fun spawnNewFolder(parent: String, name: String, target: String, host: String, agent: String, model: String, profile: String) {
        val clean = name.trim().trim('/')
        if (clean.isEmpty()) return
        client?.send(Outbound.spawnAt("$parent/$clean", create = true, target = target, host = host, agent = agent, model = model, profile = profile))
    }

    // --- Hands-free (always-listening VAD; only speech is sent) ---
    // The mic/hands-free/PTT/calibration logic lives in VoiceControllerAudio.kt; the
    // state it drives is declared here.
    internal var handsFree: HandsFreeRecorder? = null
    @Volatile internal var hfOn = false
    // Headphone state the running recorder was configured for, so a route change
    // only restarts capture when it actually flips speaker↔headphones.
    @Volatile internal var headphonesRoute = false
    // Whether we currently hold a Bluetooth headset's hands-free (SCO) profile, so
    // we only grab/release it on an actual transition (releasing resets routing).
    @Volatile internal var headsetMicOn = false
    // Latched when the headset's SCO link failed to come up (some earbuds' hands-free
    // profile won't engage on demand): capture falls back to the built-in mic and
    // stops re-attempting SCO for the rest of the session. Cleared whenever the user
    // changes the mic-source setting (so re-selecting "headset" retries) or on a fresh
    // hands-free start — see [lastMicSource].
    @Volatile internal var headsetMicFailed = false
    @Volatile internal var lastMicSource = ""

    /** Change the resident whisper model. set_whisper_model triggers the actual
     *  resident-server load/download; the server then persists the result into the synced
     *  settings catalogue and broadcasts `settings`, so the choice syncs across clients. */
    override fun setWhisperModel(model: String, fast: Boolean) = client?.send(Outbound.setWhisperModel(model, fast)).let {}

    /** Auto-compress is a synced setting now — each scalar is its own keyed record routed
     *  through the shared catalogue mutator (last-writer-wins), not a device-local write. */
    override fun setAutoCompress(warm: Boolean, auto: Boolean, thresholdK: Int) {
        catalogues.putSetting("warm_compress", warm.toString())
        catalogues.putSetting("auto_compress", auto.toString())
        catalogues.putSetting("auto_compress_threshold", thresholdK.toString())
    }

    /** Summary-only is a synced setting; mirror it locally and route it through the catalogue. */
    override fun setSummaryOnly(on: Boolean) {
        settings.summaryOnlySpeech = on
        catalogues.putSetting("summary_only", on.toString())
    }

    /** Fold the inbound shared-settings catalogue into device-local Prefs the settings UI
     *  seeds from, so a change synced from another client/server is reflected here. Whisper
     *  models drive their own StateFlows via the `whisper_model` broadcast. */
    internal fun mirrorSettingsToPrefs() {
        catalogues.settingValue("warm_compress")?.let { settings.warmCompress = it == "true" }
        catalogues.settingValue("auto_compress")?.let { settings.autoCompress = it == "true" }
        catalogues.settingValue("auto_compress_threshold")?.let { settings.autoCompressThreshold = it.toIntOrNull() ?: settings.autoCompressThreshold }
        catalogues.settingValue("summary_only")?.let { settings.summaryOnlySpeech = it == "true" }
    }

    /** Ask the server to restart. It exits so its supervisor relaunches it on
     *  current code; the app auto-reconnects once it's listening again. */
    override fun restartServer(mode: String) = client?.send(Outbound.restart(mode)).let {}

    /** Voice-picker preview: speak a short sample in [voice] through the server. */
    override fun previewTtsVoice(voice: String) {
        speakText("Hi bud — this is how I'd sound.", voice)
    }

    // --- End-token calibration state (logic in VoiceControllerAudio.kt) ---
    internal var calibRecorder: HandsFreeRecorder? = null
    internal val _calibration = MutableStateFlow(CalibrationState())
    val calibration: StateFlow<CalibrationState> = _calibration.asStateFlow()

    // Push-to-talk in-progress flag (records Opus locally; logic in VoiceControllerAudio.kt).
    @Volatile internal var recording = false

    fun shutdown() {
        if (recording) recorder.stopAndRead()
        stopCalibration()
        stopMeter()
        stopHandsFree()
        scope.cancel()
        client?.close()
        speaker.shutdown()
    }

    private fun onConnected(up: Boolean) {
        _connected.value = up
        _status.value = if (up) "connected" else "reconnecting…"
        if (!up) cancelServerSpeech() // a dropped socket orphans any in-flight speak streams
        if (!up) persist(currentKey) // flush the visible session to disk so it's available offline
        // Dropped mid-turn: arm the watchdog. If the server is alive it'll re-deliver
        // the reply (or a "still working" breadcrumb) on reconnect and disarm this;
        // if it crashed/restarted, nothing comes and we warn when the grace elapses.
        if (!up && turnInFlight) armLostTurnWatchdog()
    }

    private fun armLostTurnWatchdog() {
        // Idempotent: count the grace window from the FIRST disconnect. Auto-reconnect
        // calls onConnected(false) on every failed retry (backoff caps at 30s < the
        // 45s grace), so re-arming here would reset the timer each retry and it would
        // never fire while the server stays down — the crash case this exists for. A
        // resolving event (activity/reply/error/turn_interrupted) cancels it instead.
        if (lostTurnWatchdog?.isActive == true) return
        lostTurnWatchdog = scope.launch {
            delay(lostTurnGraceMs)
            if (turnInFlight) {
                turnInFlight = false
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                val note = "⚠️ lost the connection while working — that turn may have been interrupted. Try again if you don't hear back."
                addChat(Role.SYSTEM, note)
                speakText("that turn may have been interrupted. try again if you don't hear back.")
            }
        }
    }

    internal fun clearTurnInFlight() {
        turnInFlight = false
        lostTurnWatchdog?.cancel()
        lostTurnWatchdog = null
    }
}

package com.bam.spawner

import android.content.Context
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageReport
import com.bam.spawner.net.UsageEstimateInfo
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
 */
class VoiceController(context: Context, private val settings: SettingsStore) : AppController {
    private val app = context.applicationContext
    private val speaker = Speaker(app)
    private val recorder = OpusRecorder(app)
    private val notifier = Notifier(app)
    private var client: SpawnerClient? = null

    /** True while the app UI is in the foreground; drives whether a finished turn
     *  posts a notification. Set by the Activity's lifecycle. */
    @Volatile var appForeground = false

    private val _connected = MutableStateFlow(false)
    override val connected: StateFlow<Boolean> = _connected.asStateFlow()

    private val _status = MutableStateFlow("disconnected")
    override val status: StateFlow<String> = _status.asStateFlow()

    // Per-session chat logs, keyed by session name ("" = the general/unattached
    // view for dialog + system messages). `_chat` mirrors the current key's log.
    private val logs = mutableMapOf<String, List<ChatMessage>>()
    private val oldestIndex = mutableMapOf<String, Int>() // lowest history index held, per session
    private val hasMore = mutableMapOf<String, Boolean>() // older history remains to page, per session
    private val loadingOlder = mutableSetOf<String>()      // a page request is in flight, per session
    private val bridgeTo = mutableMapOf<String, Int>()      // reconnect gap-fill: page older until this index is reached
    private var currentKey = ""

    // Offline transcript cache. `digestHeld` is the server digest (count, hash) the
    // on-disk cache corresponds to; `serverDigest` is the latest truth the server
    // reported (connect-time `digests` sweep + every `history` reply). When the two
    // match and we hold content, an (re)attach skips the history fetch entirely.
    // `loadedFromCache` tracks which sessions we've pulled from disk into memory.
    private val cache = TranscriptCache(File(app.filesDir, "transcripts"))
    private val discoveredCache = DiscoveredCache(File(app.filesDir, "discovered.json"))
    private val digestHeld = mutableMapOf<String, Pair<Int, String>>()
    private val serverDigest = mutableMapOf<String, Pair<Int, String>>()
    private val loadedFromCache = mutableSetOf<String>()
    private var previousFocusedSession: DiscoveredInfo? = null

    // migrateSessionKey re-keys every session-name-keyed piece of client state from
    // old to new when a session is renamed, so nothing orphans under the stale name
    // (an orphaned log empties the chat; an orphaned cursor breaks paging). This is
    // the single site that must know the full set of name-keyed maps — a new one
    // added above must be migrated here too.
    private fun migrateSessionKey(old: String, new: String) {
        logs.remove(old)?.let { logs[new] = it }
        oldestIndex.remove(old)?.let { oldestIndex[new] = it }
        hasMore.remove(old)?.let { hasMore[new] = it }
        if (loadingOlder.remove(old)) loadingOlder.add(new)
        bridgeTo.remove(old)?.let { bridgeTo[new] = it }
        digestHeld.remove(old)?.let { digestHeld[new] = it }
        serverDigest.remove(old)?.let { serverDigest[new] = it }
        if (loadedFromCache.remove(old)) loadedFromCache.add(new)
        cache.remove(old) // drop the stale file; the new key is repersisted on the next persist()
    }

    // ensureLoaded pulls a session's persisted transcript from disk into the
    // in-memory maps the first time it's needed (so the cached chat shows even
    // offline), without clobbering a live in-memory log we already hold.
    private fun ensureLoaded(name: String) {
        if (name.isEmpty() || name in loadedFromCache) return
        loadedFromCache.add(name)
        if (name in logs) return
        val c = cache.load(name) ?: return
        logs[name] = dedupeCachedLog(c.messages.map { it.toChat() })
        oldestIndex[name] = c.oldestIndex
        hasMore[name] = c.hasMore
        digestHeld[name] = c.count to c.hash
    }

    private fun dedupeCachedLog(messages: List<ChatMessage>): List<ChatMessage> {
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

    // persist writes a session's current log (minus live-only SYSTEM notes, which
    // aren't part of the server transcript) plus its paging cursor and held digest
    // to disk, so it survives an app restart and can be shown offline.
    private fun persist(name: String) {
        if (name.isEmpty()) return
        val msgs = dedupeCachedLog(logs[name] ?: return)
        logs[name] = msgs
        val keep = msgs.filter { it.role != Role.SYSTEM }
        val d = digestHeld[name]
        cache.save(name, CachedSession(
            messages = keep.map { it.toCached() },
            oldestIndex = oldestIndex[name] ?: (keep.firstOrNull { it.index >= 0 }?.index ?: 0),
            hasMore = hasMore[name] ?: false,
            count = d?.first ?: 0,
            hash = d?.second ?: "",
        ))
    }

    private fun currentFocusedSession(): DiscoveredInfo? {
        val id = _attachedId.value
        val name = _attachedName.value ?: return null
        if (id.isBlank()) return null
        return _discovered.value.find { it.sessionId == id } ?: DiscoveredInfo(
            name = name,
            dir = "",
            sessionId = id,
            lastActive = 0,
            active = false,
            registered = true,
            agent = _attachedAgent.value,
            model = _attachedModel.value,
        )
    }

    private fun requestFreshHistory(name: String) {
        val held = digestHeld[name]
        val server = serverDigest[name]
        val haveContent = logs[name]?.any { it.index >= 0 } == true
        if (held != null && held == server && haveContent) return
        client?.send(Outbound.history(name, null, haveHash = held?.second ?: ""))
    }

    private fun focusKnownSession(session: DiscoveredInfo, syncServer: Boolean) {
        if (session.sessionId.isBlank()) {
            client?.send(Outbound.attach(session.name, silent = syncServer))
            return
        }
        val current = currentFocusedSession()
        if (current?.sessionId != session.sessionId) {
            current?.let { previousFocusedSession = it }
        }
        clearTurnInFlight()
        _activity.value = ""
        _pending.value = ""
        _lastTurnUsage.value = null
        _attachedId.value = session.sessionId
        _attachedName.value = session.name
        _attachedAgent.value = session.agent
        _attachedModel.value = session.model
        settings.lastSession = session.name
        settings.lastSessionId = session.sessionId
        _status.value = "attached: ${session.name}"
        showLog(session.name)
        requestFreshHistory(session.name)
        if (syncServer) client?.send(Outbound.attach(session.name, sessionId = session.sessionId, silent = true))
    }

    private val _chat = MutableStateFlow<List<ChatMessage>>(emptyList())
    override val chat: StateFlow<List<ChatMessage>> = _chat.asStateFlow()

    // Whether the current session has older history to page in (drives the
    // "load older" control), and a tick the UI watches to scroll to the bottom.
    private val _hasMoreHistory = MutableStateFlow(false)
    override val hasMoreHistory: StateFlow<Boolean> = _hasMoreHistory.asStateFlow()
    private val _scrollTick = MutableStateFlow(0)
    override val scrollTick: StateFlow<Int> = _scrollTick.asStateFlow()

    // Claude sessions found on disk (from `discover`) that can be adopted.
    // Seeded from the on-disk cache so the sidebar is populated on a fresh launch
    // before (or without) a server connection; refreshed on each connect-time sweep.
    private val _discovered = MutableStateFlow<List<DiscoveredInfo>>(discoveredCache.load())
    override val discovered: StateFlow<List<DiscoveredInfo>> = _discovered.asStateFlow()

    // Last error from a discover/adopt/delete action, shown on the Discover
    // screen (otherwise it would go to the hidden chat log). "" = none.
    private val _discoverError = MutableStateFlow("")
    override val discoverError: StateFlow<String> = _discoverError.asStateFlow()

    private val _attachedName = MutableStateFlow<String?>(null)
    override val attachedName: StateFlow<String?> = _attachedName.asStateFlow()

    // The stable on-disk id of the attached session. Names diverge between servers
    // (the same session is "spawner-2" on one, "spawner-3" on another) and change on
    // rename, so we track the id to keep the title correct: rename matches by id, and
    // a fresh session list re-derives the title from whatever the current server calls
    // this id. Exposed so the sidebar highlights the attached row by id (not name).
    // Empty when detached or when the server didn't send an id (older server).
    private val _attachedId = MutableStateFlow("")
    override val attachedId: StateFlow<String> = _attachedId.asStateFlow()

    // Backend id + model alias of the attached session (for the status-bar badge).
    private val _attachedAgent = MutableStateFlow("")
    override val attachedAgent: StateFlow<String> = _attachedAgent.asStateFlow()
    private val _attachedModel = MutableStateFlow("")
    override val attachedModel: StateFlow<String> = _attachedModel.asStateFlow()

    private val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    override val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()

    // One-shot file-transfer results (message-box 📎 button): an upload's landed path
    // and a download's bytes. SharedFlow, not StateFlow — each is a fire-once event the
    // UI reacts to (prefill the box / write the download), not retained state.
    private val _fileSaved = MutableSharedFlow<String>(extraBufferCapacity = 4)
    override val fileSaved: SharedFlow<String> = _fileSaved.asSharedFlow()
    private val _fileData = MutableSharedFlow<ServerMsg.FileData>(extraBufferCapacity = 4)
    override val fileData: SharedFlow<ServerMsg.FileData> = _fileData.asSharedFlow()

    // The app-managed SSH host registry (Settings → Hosts). The server is the store
    // of record on disk, but the app owns the list; refreshed from every host_list.
    private val _hosts = MutableStateFlow<List<com.bam.spawner.net.Host>>(emptyList())
    override val hosts: StateFlow<List<com.bam.spawner.net.Host>> = _hosts.asStateFlow()

    // The app-managed SSH identity registry (Settings → Identities); names + public
    // keys only (the server holds the private keys). Refreshed from every identity_list.
    private val _identities = MutableStateFlow<List<com.bam.spawner.net.Identity>>(emptyList())
    override val identities: StateFlow<List<com.bam.spawner.net.Identity>> = _identities.asStateFlow()

    private val _mic = MutableStateFlow("")
    val mic: StateFlow<String> = _mic.asStateFlow()

    private val _voiceState = MutableStateFlow(VoiceState.OFF)
    override val voiceState: StateFlow<VoiceState> = _voiceState.asStateFlow()

    // Live hands-free draft: what's captured but not yet committed (via end token).
    private val _pending = MutableStateFlow("")
    override val pending: StateFlow<String> = _pending.asStateFlow()

    // Live mic RMS level (~0..32768) for the audio meter, fed by the running
    // hands-free recorder or a standalone LevelMeter.
    private val _micLevel = MutableStateFlow(0.0)
    val micLevel: StateFlow<Double> = _micLevel.asStateFlow()

    // Live "Claude is thinking / editing foo.go" indicator; "" when idle.
    private val _activity = MutableStateFlow("")
    override val activity: StateFlow<String> = _activity.asStateFlow()

    // Token usage of the last completed turn, driving the per-message badge's
    // source data and the status-bar cache-warm countdown. Null until a turn
    // finishes; reset when attaching elsewhere (a new session = a new cache).
    private val _lastTurnUsage = MutableStateFlow<TurnUsageInfo?>(null)
    override val lastTurnUsage: StateFlow<TurnUsageInfo?> = _lastTurnUsage.asStateFlow()

    // The Claude plan's session-limit state (which usage window is binding, when it
    // resets), refreshed from each turn's rate_limit_event. Shown at the bottom of
    // the sessions drawer. Null until the first turn reports it.
    private val _rateLimit = MutableStateFlow<RateLimitInfo?>(null)
    override val rateLimit: StateFlow<RateLimitInfo?> = _rateLimit.asStateFlow()

    // Server-global drift-live usage estimate: nudges up each turn, snaps to real
    // on /usage. Server-wide (all sessions/clients), so it does NOT reset on attach.
    private val _usageEstimate = MutableStateFlow<UsageEstimateInfo?>(null)
    override val usageEstimate: StateFlow<UsageEstimateInfo?> = _usageEstimate.asStateFlow()

    // On-demand `/usage` report (session/weekly % used). Loading is set while the
    // server runs /usage; report holds the result. A non-null report opens the
    // usage sheet — including from the "usage" voice command (no prior request).
    private val _usageReport = MutableStateFlow<UsageReport?>(null)
    override val usageReport: StateFlow<UsageReport?> = _usageReport.asStateFlow()
    private val _usageLoading = MutableStateFlow(false)
    override val usageLoading: StateFlow<Boolean> = _usageLoading.asStateFlow()

    /** Ask the server for the plan's usage report (runs `/usage`); opens the sheet in a loading state. */
    override fun requestUsage() {
        _usageReport.value = null
        _usageLoading.value = true
        client?.send(Outbound.usage())
    }

    /** "set" button: stamp the current odometer + real percentages as the two-point benchmark start. */
    override fun setUsageBenchmark() {
        _usageReport.value = null
        _usageLoading.value = true
        client?.send(Outbound.usageSet())
    }

    /** "calc" button: derive the tokens-per-percent rate directly from the benchmark interval. */
    override fun calcUsageMax() {
        _usageReport.value = null
        _usageLoading.value = true
        client?.send(Outbound.usageCalc())
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
    private val _whisperModel = MutableStateFlow(settings.whisperModel)
    override val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()

    // The fast (draft/detection, "quick") server's model; "" = none configured.
    private val _whisperFastModel = MutableStateFlow(settings.whisperFastModel)
    override val whisperFastModel: StateFlow<String> = _whisperFastModel.asStateFlow()

    // Catalogue offered by the picker; not persisted — re-sent on connect.
    private val _whisperModels = MutableStateFlow<List<String>>(emptyList())
    override val whisperModels: StateFlow<List<String>> = _whisperModels.asStateFlow()

    // Which catalogue models are already downloaded on the server.
    private val _whisperModelsLocal = MutableStateFlow<List<String>>(emptyList())
    override val whisperModelsLocal: StateFlow<List<String>> = _whisperModelsLocal.asStateFlow()

    // Whether the connected server offers Kokoro TTS (hello_ok `tts`).
    private val _serverTtsAvailable = MutableStateFlow(false)
    override val serverTtsAvailable: StateFlow<Boolean> = _serverTtsAvailable.asStateFlow()

    // Kokoro's voice catalogue + server default (tts_voices reply; feeds the picker).
    private val _ttsVoices = MutableStateFlow<List<String>>(emptyList())
    override val ttsVoices: StateFlow<List<String>> = _ttsVoices.asStateFlow()
    private val _ttsVoiceDefault = MutableStateFlow("")
    override val ttsVoiceDefault: StateFlow<String> = _ttsVoiceDefault.asStateFlow()

    // --- Server-TTS (Kokoro) speak bookkeeping --------------------------------
    // Everything the net thread and the UI thread both touch sits under speakLock.
    private val speakLock = Any()
    private var speakSeq = 0L
    // id -> stripped text of each in-flight speak, kept for the on-device
    // fallback when the server refuses (error-bearing speak_end).
    private val speakTexts = LinkedHashMap<String, String>()
    private var speakStreamId: String? = null // utterance whose binary frames are arriving
    private var speakStreamLive = false // false = cancelled/foreign: drop its remaining frames

    // Live model-download progress; null when no fetch is in flight.
    private val _whisperDownload = MutableStateFlow<WhisperDownloadInfo?>(null)
    override val whisperDownload: StateFlow<WhisperDownloadInfo?> = _whisperDownload.asStateFlow()

    // Pending clarification questions (interactive mode); null when none.
    private val _ask = MutableStateFlow<List<com.bam.spawner.net.AskQuestion>?>(null)
    override val ask: StateFlow<List<com.bam.spawner.net.AskQuestion>?> = _ask.asStateFlow()

    // AI backend registry from the `agents` message (backends + models for the picker).
    private val _agents = MutableStateFlow<List<com.bam.spawner.net.AgentInfo>>(emptyList())
    override val agents: StateFlow<List<com.bam.spawner.net.AgentInfo>> = _agents.asStateFlow()
    private val _profiles = MutableStateFlow<List<ProfileInfo>>(emptyList())
    override val profiles: StateFlow<List<ProfileInfo>> = _profiles.asStateFlow()

    // Spoken-audio output routing (earpiece/speaker/bluetooth). `audioOutputs` is
    // what's currently selectable (bluetooth only when a headset is connected);
    // `audioOutput` is the active one.
    private val audioRouter = AudioRouter(app)
    private val _audioOutputs = MutableStateFlow(listOf(AudioOutput.EARPIECE, AudioOutput.SPEAKER))
    val audioOutputs: StateFlow<List<AudioOutput>> = _audioOutputs.asStateFlow()
    private val _audioOutput = MutableStateFlow(AudioOutput.EARPIECE)
    val audioOutput: StateFlow<AudioOutput> = _audioOutput.asStateFlow()
    // Capture (mic) source, picked independently of the output: `audioInputs` is
    // what's currently selectable (headset mic only when a Bluetooth headset is
    // paired); `audioInput` is the active one.
    private val _audioInputs = MutableStateFlow(listOf(AudioInput.DEVICE))
    val audioInputs: StateFlow<List<AudioInput>> = _audioInputs.asStateFlow()
    private val _audioInput = MutableStateFlow(AudioInput.DEVICE)
    val audioInput: StateFlow<AudioInput> = _audioInput.asStateFlow()

    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private var meter: LevelMeter? = null
    private var commitTimer: Job? = null

    // A dictation turn is "in flight" from the server's first activity breadcrumb
    // until its reply/error. If the connection drops mid-turn and nothing resolves
    // it within the grace window, we warn the user rather than spin forever — a
    // turn that dies with a server restart/crash otherwise gives no notice.
    @Volatile private var turnInFlight = false
    private var lostTurnWatchdog: Job? = null
    private val lostTurnGraceMs = 45_000L
    // Sessions that have streamed at least one live prose chunk for their current
    // turn. Keyed by session name so a gesture swap cannot make a late frame from
    // the previous session render into the newly visible log.
    private val streamedSessions = mutableSetOf<String>()

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

    // Apply an output: MUTE suppresses TTS (no device routing); anything else
    // unmutes and routes the device. Returns whether it took effect.
    private fun applyAudioOutput(out: AudioOutput): Boolean =
        if (out == AudioOutput.MUTE) { cancelServerSpeech(); speaker.setMuted(true); true }
        else { speaker.setMuted(false); audioRouter.setOutput(out) }

    /** Re-scan available outputs (call when opening the picker to catch a
     *  just-connected/removed Bluetooth headset). */
    fun refreshAudioOutputs() {
        val avail = audioRouter.available()
        _audioOutputs.value = avail
        _audioInputs.value = audioRouter.availableInputs()
        // If the selected device vanished (e.g. the Bluetooth headset disconnected),
        // fall back to earpiece. MUTE is always available.
        val cur = _audioOutput.value
        if (cur != AudioOutput.MUTE && cur !in avail) setAudioOutput(AudioOutput.EARPIECE)
        // Likewise, if the headset mic went away, fall back to the device mic.
        if (_audioInput.value == AudioInput.HEADSET && AudioInput.HEADSET !in _audioInputs.value) {
            setAudioInput(AudioInput.DEVICE)
        }
    }

    /** Choose the capture (mic) source and remember it. Capture is route-dependent,
     *  so re-resolve the mic profile live while listening. */
    fun setAudioInput(inp: AudioInput) {
        _audioInput.value = inp
        settings.micSource = inp.pref
        // An explicit pick is a deliberate (re)try, so clear any prior SCO-failure
        // latch: re-selecting Headset must re-attempt the Bluetooth link rather than
        // stay silently on the built-in mic (the latch otherwise only clears when the
        // input *value* changes, forcing a Device→Headset round-trip).
        headsetMicFailed = false
        if (hfOn) restartHandsFree()
        _audioInputs.value = audioRouter.availableInputs()
    }

    /** Route the spoken audio to [out] (or mute) and remember the choice. */
    fun setAudioOutput(out: AudioOutput) {
        if (applyAudioOutput(out)) {
            _audioOutput.value = out
            settings.audioOutput = out.name.lowercase()
            // Capture is route-dependent (comm-audio vs media, headset vs built-in mic),
            // so re-resolve the mic profile against the new output while listening.
            if (hfOn) restartHandsFree()
        }
        _audioOutputs.value = audioRouter.available()
    }

    fun connect(url: String, token: String) {
        client?.close()
        _status.value = "connecting…"
        val hello = com.bam.spawner.net.HelloConfig(
            settings.endToken, settings.wakeToken, settings.speakToken, settings.dictationGate,
            settings.sttMode, settings.sttModel, settings.aliasMap(),
            settings.whisperUrl, settings.brief, settings.interactive,
            settings.warmCompress, settings.autoCompress, settings.autoCompressThreshold,
        )
        client = SpawnerClient(url, token, settings.clientId, hello, ::onMessage, ::onConnected, ::onSpeakFrame)
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
    private fun scrollToBottom() { _scrollTick.value = _scrollTick.value + 1 }

    // --- Sidebar actions ---

    /** Ask the server for all Claude sessions on disk (spawner-created or not). */
    override fun discover() = client?.send(Outbound.discover()).let {}

    /** Request the SSH host registry (Settings → Hosts). Server replies host_list. */
    override fun requestHosts() = client?.send(Outbound.hostsList()).let {}

    /** Add or update a host; the server broadcasts the refreshed host_list. */
    override fun putHost(host: com.bam.spawner.net.Host) = client?.send(Outbound.hostPut(host)).let {}

    /** Delete a host by name; the server broadcasts the refreshed host_list. */
    override fun deleteHost(name: String) = client?.send(Outbound.hostDelete(name)).let {}

    /** Request the SSH identity registry (Settings → Identities). Replies identity_list. */
    override fun requestIdentities() = client?.send(Outbound.identitiesList()).let {}

    /** Create a new identity (user required; optional keypair + password); broadcasts identity_list. */
    override fun createIdentity(name: String, user: String, password: String, genKey: Boolean) {
        client?.send(Outbound.identityCreate(name, user, password, genKey))
    }

    /** Import an existing server-side private key as an identity; broadcasts identity_list. */
    override fun importIdentity(name: String, user: String, password: String, keyPath: String) {
        client?.send(Outbound.identityImport(name, user, password, keyPath))
    }

    /** Update an identity's user (and optionally its password), keeping the keypair. */
    override fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) {
        client?.send(Outbound.identityUpdate(name, user, setPassword, password))
    }

    /** Delete an identity by name; broadcasts identity_list. */
    override fun deleteIdentity(name: String) = client?.send(Outbound.identityDelete(name)).let {}

    /** Add or update an execution profile; the server broadcasts the refreshed profiles. */
    override fun putProfile(p: com.bam.spawner.net.ProfileInfo) = client?.send(Outbound.profilePut(p)).let {}

    /** Delete a profile by name; broadcasts profiles. */
    override fun deleteProfile(name: String) = client?.send(Outbound.profileDelete(name)).let {}

    /** Mark a profile as the default; broadcasts profiles. */
    override fun setDefaultProfile(name: String) = client?.send(Outbound.profileSetDefault(name)).let {}

    /** Set a backend's default model + voice-enumerable models; broadcasts agents. */
    override fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>) =
        client?.send(Outbound.providerPut(agent, defaultModel, voiceModels)).let {}

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

    /** Request the page of history just older than what we hold for `name`. Shared by
     *  the user's scroll-back (loadOlder) and the reconnect gap-fill in onHistory. */
    private fun fetchOlder(name: String) {
        if (name.isEmpty() || hasMore[name] != true || name in loadingOlder) return
        val before = oldestIndex[name] ?: return
        loadingOlder.add(name)
        client?.send(Outbound.history(name, before))
    }

    override fun detach() {
        currentFocusedSession()?.let { previousFocusedSession = it }
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
        val target = previousFocusedSession
        if (target == null || target.sessionId.isBlank()) {
            client?.send(Outbound.swap())
            return
        }
        val refreshed = _discovered.value.firstOrNull { it.sessionId == target.sessionId }
        if (refreshed == null && _discovered.value.isNotEmpty()) {
            previousFocusedSession = null
            _status.value = "previous session is gone"
            return
        }
        focusKnownSession(refreshed ?: target, syncServer = true)
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

    private fun spokenQuestions(qs: List<AskQuestion>): String {
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
    private var handsFree: HandsFreeRecorder? = null
    @Volatile private var hfOn = false
    // Headphone state the running recorder was configured for, so a route change
    // only restarts capture when it actually flips speaker↔headphones.
    @Volatile private var headphonesRoute = false
    // Whether we currently hold a Bluetooth headset's hands-free (SCO) profile, so
    // we only grab/release it on an actual transition (releasing resets routing).
    @Volatile private var headsetMicOn = false
    // Latched when the headset's SCO link failed to come up (some earbuds' hands-free
    // profile won't engage on demand): capture falls back to the built-in mic and
    // stops re-attempting SCO for the rest of the session. Cleared whenever the user
    // changes the mic-source setting (so re-selecting "headset" retries) or on a fresh
    // hands-free start — see [lastMicSource].
    @Volatile private var headsetMicFailed = false
    @Volatile private var lastMicSource = ""

    private fun vadConfig() = com.bam.spawner.audio.VadConfig(
        rmsThreshold = settings.vadThreshold.toDouble(),
        onsetMs = settings.vadOnsetMs,
        silenceMs = settings.vadSilenceMs,
        adaptive = settings.vadAdaptive,
    )

    // How to capture + play during hands-free, resolved from the current output route.
    private data class MicProfile(val commMode: Boolean, val source: Int, val aec: Boolean, val ns: Boolean)

    /** Resolve how to capture + play for hands-free straight from the two explicit
     *  picks — the [AudioInput] mic source and the [AudioOutput] route — with no
     *  inference:
     *  - Headset input + a Bluetooth headset present → grab its hands-free (SCO)
     *    profile: comm audio + AEC, headset mic, from across the room (call quality).
     *    The SCO link carries playback too, so it overrides the output route.
     *  - Device input + headset output → plain media capture, no AEC: our TTS is in
     *    the user's ears (nothing to echo-cancel) and staying out of call mode stops
     *    Android from ducking other apps' audio (e.g. a movie) to a whisper.
     *  - Device input + earpiece/speaker/mute → comm audio + echo canceller so voice
     *    barge-in works over the speaker. */
    private fun resolveMicProfile(): MicProfile {
        // Any change to the input pick clears a prior SCO-failure latch, so explicitly
        // re-selecting the headset retries it; an unchanged value (e.g. a route-change
        // restart) keeps the latch so we don't loop on a dead link.
        val inputPref = _audioInput.value.pref
        if (inputPref != lastMicSource) { headsetMicFailed = false; lastMicSource = inputPref }
        val useHeadset =
            _audioInput.value == AudioInput.HEADSET && !headsetMicFailed && audioRouter.bluetoothMicAvailable()
        if (useHeadset && !headsetMicOn) { headsetMicOn = audioRouter.enableHeadsetMic() }
        else if (!useHeadset && headsetMicOn) { audioRouter.disableHeadsetMic(); headsetMicOn = false }
        headphonesRoute = audioRouter.headphonesConnected()
        // Headset mic (SCO) → call-mode capture. Otherwise the device mic, whose
        // profile follows the output route.
        return when {
            headsetMicOn -> MicProfile(true, android.media.MediaRecorder.AudioSource.VOICE_COMMUNICATION, true, true)
            // Headset/media path: AEC stays off (TTS is in the user's ears), but the
            // noise suppressor is an independent opt-in for filtering ambient noise.
            _audioOutput.value == AudioOutput.HEADSET ->
                MicProfile(false, android.media.MediaRecorder.AudioSource.VOICE_RECOGNITION, false, settings.headsetNoiseSuppression)
            else -> MicProfile(true, android.media.MediaRecorder.AudioSource.VOICE_COMMUNICATION, true, true)
        }
    }

    private fun newHandsFree(profile: MicProfile) = HandsFreeRecorder(
        app, vadConfig(), ::onHandsFreeSpeechStart, ::onHandsFreeUtterance,
        { _micLevel.value = it }, profile.source, profile.aec, profile.ns,
    )

    /** Starts the always-listening pipeline. Returns false if the mic is unavailable. */
    fun startHandsFree(): Boolean {
        if (hfOn) return true
        if (recording) return false // don't fight push-to-talk for the mic
        stopMeter() // free the mic if the level meter was running
        headsetMicFailed = false // a fresh enable gets one clean SCO attempt
        val profile = resolveMicProfile()
        val hf = newHandsFree(profile)
        if (!hf.start()) {
            _mic.value = "⚠️ mic unavailable"
            return false
        }
        handsFree = hf
        hfOn = true
        speaker.setCommMode(profile.commMode)
        _voiceState.value = VoiceState.LISTENING
        _mic.value = "🟢 listening for \"hey buddy\"…"
        if (headsetMicOn) scope.launch { verifyHeadsetMic() }
        return true
    }

    /** Re-apply VAD settings / audio route live (restart the recorder) if hands-free
     *  is running. */
    fun restartHandsFree() {
        if (!hfOn) return
        handsFree?.stop()
        val profile = resolveMicProfile()
        val hf = newHandsFree(profile)
        if (hf.start()) {
            handsFree = hf
            speaker.setCommMode(profile.commMode)
            _voiceState.value = VoiceState.LISTENING
            if (headsetMicOn) scope.launch { verifyHeadsetMic() }
        } else {
            handsFree = null; hfOn = false; _voiceState.value = VoiceState.OFF
        }
    }

    /** After grabbing a Bluetooth headset's hands-free profile, give the SCO link a
     *  moment to actually come up. If it didn't (some earbuds refuse it on demand and
     *  the platform silently reverts to the mic-less A2DP link), latch the failure and
     *  restart on the built-in mic so the user is never left unheard. */
    private suspend fun verifyHeadsetMic() {
        // Poll the SCO link rather than judging it once: car kits and some earbuds
        // take several seconds to bring the hands-free profile up, and a single early
        // check wrongly latched failure and dropped to the built-in mic. Give it up to
        // ~6 s, succeeding the moment the link is live.
        repeat(12) {
            delay(500)
            if (!hfOn || !headsetMicOn) return
            if (audioRouter.headsetMicActive()) return // link came up — keep the headset mic
        }
        headsetMicFailed = true // gave it a fair window; fall back so the user is heard
        _mic.value = "🟢 listening (headset mic unavailable — using built-in)…"
        restartHandsFree()
    }

    /** Headphones plugged/unplugged (or Bluetooth connected/dropped): if the route
     *  actually flipped speaker↔headphones while listening, restart capture so the
     *  comm-mode/echo-canceller choice follows it. Runs off the main thread because
     *  restarting joins the capture worker. */
    private fun onAudioRouteChanged() {
        val nowHeadphones = audioRouter.headphonesConnected()
        _audioOutputs.value = audioRouter.available()
        _audioInputs.value = audioRouter.availableInputs()
        // If the headset mic was selected but its headset is gone, fall back to the
        // device mic (setAudioInput restarts capture).
        if (_audioInput.value == AudioInput.HEADSET && AudioInput.HEADSET !in _audioInputs.value) {
            setAudioInput(AudioInput.DEVICE); return
        }
        // Auto-prefer the headset-media output when a headset appears — but only if the
        // user is still on the default earpiece, so an explicit pick is left untouched;
        // fall back off it when the headset goes away. setAudioOutput restarts capture.
        if (nowHeadphones && _audioOutput.value == AudioOutput.EARPIECE) {
            setAudioOutput(AudioOutput.HEADSET); return
        }
        if (!nowHeadphones && _audioOutput.value == AudioOutput.HEADSET) {
            setAudioOutput(AudioOutput.EARPIECE); return
        }
        if (!hfOn) { headphonesRoute = nowHeadphones; return }
        if (nowHeadphones == headphonesRoute) return
        restartHandsFree()
    }

    fun stopHandsFree() {
        hfOn = false
        speaker.setCommMode(false) // back to the regular media speaker
        handsFree?.stop()
        handsFree = null
        if (headsetMicOn) { // release the Bluetooth hands-free profile we grabbed
            audioRouter.disableHeadsetMic(); headsetMicOn = false
            applyAudioOutput(_audioOutput.value) // restore the user's chosen TTS output
        }
        cancelSilenceCommit()
        _micLevel.value = 0.0
        _voiceState.value = VoiceState.OFF
        _mic.value = ""
        // Drop any uncommitted draft: clear it on-screen now, and tell the server
        // to discard its buffered audio so it can't bleed into the next capture.
        if (_pending.value.isNotEmpty()) {
            _pending.value = ""
            if (_connected.value) client?.send(Outbound.discardDraft())
        }
    }

    /** Stop the TTS readout (the on-screen tap-to-stop). */
    fun stopSpeaking() {
        cancelServerSpeech()
        speaker.stop()
    }

    // --- Server-TTS (Kokoro) playback ------------------------------------------

    /** Speak [text] (already markdown-stripped): with the server's Kokoro voice
     *  when the toggle is on and the server offers TTS, else on-device. The
     *  server streams PCM back bracketed by speak_audio/speak_end; an
     *  error-bearing speak_end falls back to the on-device voice. */
    private fun speakText(text: String) = speakText(text, settings.ttsVoice)

    private fun speakText(text: String, voice: String) {
        if (text.isBlank() || speaker.isMuted()) return
        if (settings.serverTts && _serverTtsAvailable.value && _connected.value) {
            val id = synchronized(speakLock) {
                val id = "s${++speakSeq}"
                speakTexts[id] = text
                // Runaway guard; the server refuses past 32 queued anyway.
                while (speakTexts.size > 64) speakTexts.remove(speakTexts.keys.first())
                id
            }
            client?.send(Outbound.speak(id, text, voice = voice, format = "pcm"))
        } else {
            speaker.speak(text)
        }
    }

    /** Voice-picker preview: speak a short sample in [voice] through the server. */
    override fun previewTtsVoice(voice: String) {
        speakText("Hi bud — this is how I'd sound.", voice)
    }

    /** speak_audio: the next binary frames are this utterance's PCM. Anything we
     *  didn't ask for (or a codec we can't stream) is dropped and falls back on
     *  its speak_end. */
    private fun onSpeakAudio(msg: ServerMsg.SpeakAudio) = synchronized(speakLock) {
        speakStreamId = msg.id
        speakStreamLive = msg.codec == "pcm" && speakTexts.containsKey(msg.id)
        if (speakStreamLive) speaker.streamBegin()
    }

    /** A server→client binary frame — always speak audio (the only binary the
     *  server sends; ordered on the same socket as its speak_audio header). */
    private fun onSpeakFrame(data: ByteArray) {
        val live = synchronized(speakLock) { speakStreamLive }
        if (live) speaker.streamWrite(data)
    }

    private fun onSpeakEnd(msg: ServerMsg.SpeakEnd) {
        val wasLive: Boolean
        val text: String?
        synchronized(speakLock) {
            wasLive = speakStreamLive && speakStreamId == msg.id
            if (speakStreamId == msg.id) {
                speakStreamId = null
                speakStreamLive = false
            }
            text = speakTexts.remove(msg.id)
        }
        if (wasLive) speaker.streamEnd()
        // Refused (tts disabled / queue full / synthesis failed) → on-device voice.
        // A stream that died part-way (wasLive) already spoke partially; don't
        // replay the whole utterance on top of it.
        if (msg.error.isNotEmpty() && text != null && !wasLive) speaker.speak(text)
    }

    /** Forget all in-flight server speaks and silence their playback (barge-in,
     *  mute, disconnect). Frames still arriving for a cancelled utterance are
     *  dropped until its speak_end passes; speak_stop tells the server to drop
     *  its queue and abort the in-flight synthesis too (moot when disconnected —
     *  the outbox just drops it). */
    private fun cancelServerSpeech() {
        val hadInFlight = synchronized(speakLock) {
            val had = speakTexts.isNotEmpty() || speakStreamLive
            speakTexts.clear()
            speakStreamLive = false
            had
        }
        if (hadInFlight) client?.send(Outbound.speakStop())
        speaker.streamStop()
    }

    /** Change the resident whisper model (server-global; the server broadcasts the
     *  new value back to every client). */
    override fun setWhisperModel(model: String, fast: Boolean) = client?.send(Outbound.setWhisperModel(model, fast)).let {}

    /** Push the auto-compress preference to the server (server-global; live). */
    override fun setAutoCompress(warm: Boolean, auto: Boolean, thresholdK: Int) =
        client?.send(Outbound.autoCompress(warm, auto, thresholdK)).let {}

    /** Ask the server to restart. It exits so its supervisor relaunches it on
     *  current code; the app auto-reconnects once it's listening again. */
    override fun restartServer(rebuild: Boolean) = client?.send(Outbound.restart(rebuild)).let {}

    // --- Live level meter (Audio settings page) ---
    /** Start a standalone meter unless hands-free is already feeding the level. */
    fun startMeter() {
        if (hfOn || meter != null) return
        val m = LevelMeter(app) { _micLevel.value = it }
        if (m.start()) meter = m
    }

    fun stopMeter() {
        meter?.stop(); meter = null
        if (!hfOn) _micLevel.value = 0.0
    }

    // --- Silence-commit timeout (client-driven): commit the buffer after N s quiet ---
    private fun scheduleSilenceCommit() {
        cancelSilenceCommit()
        val secs = settings.silenceCommitSeconds
        if (secs <= 0f) return
        commitTimer = scope.launch {
            delay((secs * 1000).toLong())
            client?.send(Outbound.commit())
        }
    }

    private fun cancelSilenceCommit() {
        commitTimer?.cancel(); commitTimer = null
    }

    // --- End-token calibration: say the token N times, measure recognition ---
    private var calibRecorder: HandsFreeRecorder? = null
    private val _calibration = MutableStateFlow(CalibrationState())
    val calibration: StateFlow<CalibrationState> = _calibration.asStateFlow()

    fun startCalibration(rounds: Int = 10) {
        stopCalibration()
        if (hfOn) stopHandsFree() // free the mic
        val token = settings.endToken
        val rec = HandsFreeRecorder(app, vadConfig(), onSpeechStart = {}, onUtterance = { clip ->
            client?.let { c ->
                c.send(Outbound.wake(HandsFreeRecorder.CODEC, calibrate = true))
                c.sendAudio(clip)
                c.send(Outbound.audioEnd())
            }
        })
        if (rec.start()) {
            calibRecorder = rec
            _calibration.value = CalibrationState(active = true, token = token, rounds = rounds)
        } else {
            _mic.value = "⚠️ mic unavailable"
        }
    }

    fun stopCalibration() {
        calibRecorder?.stop(); calibRecorder = null
        val st = _calibration.value
        if (st.active) _calibration.value = st.copy(active = false, done = st.samples.isNotEmpty())
    }

    private fun onCalibrationSample(text: String) {
        val st = _calibration.value
        if (!st.active) return
        val samples = st.samples + text
        val hits = samples.count { endTokenHit(it, st.token) }
        if (samples.size >= st.rounds) {
            calibRecorder?.stop(); calibRecorder = null
            _calibration.value = st.copy(active = false, done = true, samples = samples, hits = hits)
        } else {
            _calibration.value = st.copy(samples = samples, hits = hits)
        }
    }

    /** Whole-word match mirroring the server's splitEndToken. */
    private fun endTokenHit(transcript: String, token: String): Boolean {
        val tok = words(token)
        if (tok.isEmpty()) return false
        val ws = words(transcript)
        var i = 0
        while (i + tok.size <= ws.size) {
            if (ws.subList(i, i + tok.size) == tok) return true
            i++
        }
        return false
    }

    private fun words(s: String): List<String> =
        s.lowercase().replace(Regex("[,.!?;:\"]"), " ").split(Regex("\\s+")).filter { it.isNotBlank() }

    // Called on the capture thread when the user starts speaking.
    private fun onHandsFreeSpeechStart() {
        cancelSilenceCommit() // still talking — don't silence-commit
        // No auto barge-in: speaking does NOT cut off Claude's reply. Only the
        // explicit "hey buddy stop" command halts speech (see ServerMsg.StopSpeaking).
        _voiceState.value = VoiceState.CAPTURING
    }

    // Called on the capture thread with a finished Opus clip; send it like PTT.
    private fun onHandsFreeUtterance(clip: ByteArray) {
        val c = client ?: return
        c.send(Outbound.wake(HandsFreeRecorder.CODEC, handsFree = true, sessionId = _attachedId.value))
        c.sendAudio(clip)
        c.send(Outbound.audioEnd())
        scheduleSilenceCommit() // start the quiet-timeout after this utterance
        // Back to listening; a server Transcript will bump us to THINKING if acted on.
        if (hfOn) _voiceState.value = VoiceState.LISTENING
    }

    // --- Push-to-talk (records Opus locally, sends the compressed clip on release) ---
    @Volatile private var recording = false

    fun startTalking() {
        val c = client
        if (c == null || !_connected.value) {
            _mic.value = "⚠️ connect first"
            return
        }
        if (hfOn) return // hands-free owns the mic
        cancelServerSpeech()
        speaker.stop() // barge-in
        if (!recorder.start()) {
            _mic.value = "⚠️ mic unavailable"
            return
        }
        recording = true
        c.send(Outbound.wake(OpusRecorder.CODEC, sessionId = _attachedId.value))
        _mic.value = "🎙️ recording…"
    }

    fun stopTalking() {
        if (!recording) return
        recording = false
        val clip = recorder.stopAndRead()
        if (clip != null && clip.isNotEmpty()) {
            client?.sendAudio(clip)
            _mic.value = "sent ${clip.size / 1024} KB — transcribing…"
        } else {
            _mic.value = "⚠️ no audio captured"
        }
        client?.send(Outbound.audioEnd())
    }

    /** Abort an in-progress push-to-talk without sending the clip — used when a
     *  swipe-up on the mic button reinterprets the hold as a hands-free toggle.
     *  We don't send `audio_end`; the server's collecting flag self-heals on the
     *  next `wake`, and skipping it avoids a spurious "didn't hear anything." */
    fun cancelTalking() {
        if (!recording) return
        recording = false
        recorder.stopAndRead() // discard the captured audio
        _mic.value = ""
    }

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

    private fun clearTurnInFlight() {
        turnInFlight = false
        lostTurnWatchdog?.cancel()
        lostTurnWatchdog = null
    }

    private fun onMessage(msg: ServerMsg) {
        // Exhaustive over the ServerMsg sealed interface — deliberately NO `else`
        // branch, so adding a new server message fails to compile until it's handled
        // here. Unknown (an unrecognized wire type) is its own explicit no-op case;
        // don't collapse it into an `else` or that compile-time guard is lost.
        when (msg) {
            is ServerMsg.HelloOk -> {
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
            is ServerMsg.WhisperModel -> {
                if (msg.model.isNotBlank()) { _whisperModel.value = msg.model; settings.whisperModel = msg.model }
                _whisperFastModel.value = msg.fastModel
                settings.whisperFastModel = msg.fastModel
                if (msg.models.isNotEmpty()) _whisperModels.value = msg.models
                _whisperModelsLocal.value = msg.local
            }
            is ServerMsg.WhisperDownload -> {
                // Clear the banner once a download completes cleanly; keep it on error so
                // the failure is visible, and while in flight to drive the progress bar.
                _whisperDownload.value =
                    if (msg.done && msg.error.isBlank()) null
                    else WhisperDownloadInfo(msg.model, msg.fast, msg.received, msg.total, msg.done, msg.error)
            }
            is ServerMsg.Say -> {
                // A `say` is also the terminal event for a background turn that has no
                // spoken Claude reply — notably `compress`, which finishes with a
                // confirmation `say` rather than an `output`. Clear the in-flight/activity
                // state so the "…compressing… ⏹ stop" bar dismisses; otherwise it lingers
                // and tapping stop aborts an already-finished turn ("nothing running to stop").
                clearTurnInFlight()
                _activity.value = ""
                _mic.value = "" // a terminal `say` (e.g. "didn't catch that") ends the PTT clip; clear "transcribing…"
                addChat(Role.SYSTEM, msg.text); speakText(Markdown.toSpeech(msg.text))
            }
            is ServerMsg.Output -> {
                // Summary-only mode: don't read the intermediate streamed steps aloud —
                // play a soft beep as a "still working…" cue and speak only the final
                // result when the turn closes. Everything is still shown in the chat.
                val summaryOnly = settings.summaryOnlySpeech
                if (msg.chunk) {
                    // A live segment of Claude's reply as it's produced. Show it now; a
                    // streamed chunk also proves the turn survived (like activity), so
                    // keep it in flight and disarm the interruption watchdog.
                    turnInFlight = true
                    lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
                    streamedSessions.add(msg.name)
                    _activity.value = "" // prose is arriving — drop the "thinking" breadcrumb
                    addChat(Role.CLAUDE, msg.text, key = msg.name)
                    if (msg.name == currentKey) {
                        if (summaryOnly) speaker.beep() else speakText(Markdown.toSpeech(msg.text))
                    }
                } else {
                    clearTurnInFlight()
                    _activity.value = "" // turn done — stop the thinking indicator
                    if (!streamedSessions.remove(msg.name)) { // no live stream reached us (buffered reply on reconnect)
                        addChat(Role.CLAUDE, msg.text, msg.usage, key = msg.name)
                        if (msg.name == currentKey) speakText(Markdown.toSpeech(msg.text))
                    } else {
                        // Streamed turn: the bubble already exists from chunks, so badge it
                        // in place — the closing message isn't re-rendered as a new bubble.
                        if (msg.usage != null) attachUsageToLastClaude(msg.name, msg.usage)
                        // In summary-only mode the chunks only beeped, so speak the final
                        // result now (the closing message carries the full reply text).
                        if (summaryOnly && msg.name == currentKey) speakText(Markdown.toSpeech(msg.text))
                    }
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
            is ServerMsg.ContextReset -> _lastTurnUsage.value = null // context cleared → status bar returns to 0
            is ServerMsg.Activity -> {
                // A live breadcrumb means the turn is running server-side; mark it in
                // flight and disarm any interruption watchdog (it survived a reconnect).
                turnInFlight = true
                lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
                _activity.value = msg.text
            }
            is ServerMsg.Transcribing -> {
                // A committed hands-free clip is being re-transcribed accurately.
                // Show "transcribing…" instead of flashing back to "listening" until
                // the transcript lands (which flips this to "thinking…").
                if (hfOn) _voiceState.value = VoiceState.TRANSCRIBING
            }
            is ServerMsg.Files -> if (msg.files.isNotEmpty()) {
                // A changed-files note hits the chat like any intermediate step, so in
                // summary-only mode it beeps too — otherwise these slip by silently.
                addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "))
                if (settings.summaryOnlySpeech) speaker.beep()
            }
            is ServerMsg.Diff -> {
                addChat(Role.SYSTEM, "📊 diff:\n${msg.text}") // review summary, not spoken
                if (settings.summaryOnlySpeech) speaker.beep()
            }
            is ServerMsg.RateLimit -> _rateLimit.value = msg.info // plan session-limit readout (sidebar)
            is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report } // opens the usage sheet
            is ServerMsg.UsageEstimate -> _usageEstimate.value = msg.est // drift-live footer/sheet estimate
            is ServerMsg.Ask -> {
                clearTurnInFlight()
                streamedSessions.remove(msg.name)
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                _ask.value = msg.questions
                addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q }, key = msg.name)
                speakText(spokenQuestions(msg.questions)) // read aloud so you can answer by voice
            }
            is ServerMsg.Transcript -> {
                _ask.value = null // a spoken/typed reply answers any pending questions
                (_attachedName.value ?: currentKey).takeIf { it.isNotEmpty() }?.let { streamedSessions.remove(it) }
                addChat(Role.USER, msg.text); _mic.value = ""
                // Chirp the "heard you" acknowledgment: the server has recognized the
                // utterance and is dispatching it to the session, so confirm receipt
                // now — before Claude replies (and distinct from its activity beep).
                if (msg.final) speaker.chirp()
                if (hfOn) _voiceState.value = VoiceState.THINKING
            }
            is ServerMsg.Pending -> {
                _pending.value = msg.text
                if (msg.text.isEmpty()) cancelSilenceCommit() // committed/cleared
                if (hfOn) _voiceState.value = if (msg.text.isEmpty()) VoiceState.LISTENING else VoiceState.CAPTURING
            }
            is ServerMsg.Calibration -> onCalibrationSample(msg.text)
            is ServerMsg.StopSpeaking -> {
                cancelServerSpeech()
                speaker.stop()
            }
            is ServerMsg.SpeakAudio -> onSpeakAudio(msg)
            is ServerMsg.SpeakEnd -> onSpeakEnd(msg)
            is ServerMsg.TtsVoices -> if (msg.error.isEmpty()) {
                _ttsVoices.value = msg.voices
                _ttsVoiceDefault.value = msg.defaultVoice
            }
            is ServerMsg.SpeechMode -> settings.summaryOnlySpeech = msg.summaryOnly // "summary only" / "speak everything" voice toggle
            is ServerMsg.Dialog -> _status.value = "dialog: ${msg.state}"
            is ServerMsg.Attached -> {
                val sameLogicalSession = _attachedName.value == msg.name
                if (_attachedId.value.isNotEmpty() && _attachedId.value != msg.sessionId && !sameLogicalSession) {
                    currentFocusedSession()?.let { previousFocusedSession = it }
                }
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
                requestFreshHistory(msg.name)
            }
            is ServerMsg.Detached -> {
                currentFocusedSession()?.let { previousFocusedSession = it }
                _attachedId.value = ""
                _attachedName.value = null
                _attachedAgent.value = ""
                _attachedModel.value = ""
                settings.lastSession = ""
                settings.lastSessionId = ""
                _status.value = "connected"
                showLog("")
            }
            is ServerMsg.Renamed -> {
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
            is ServerMsg.History -> onHistory(msg)
            is ServerMsg.ReadLast -> onReadLast(msg.count)
            is ServerMsg.Discovered -> {
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
            is ServerMsg.Listing -> _listing.value = msg
            is ServerMsg.FileSaved -> _fileSaved.tryEmit(msg.path)
            is ServerMsg.FileData -> _fileData.tryEmit(msg)
            is ServerMsg.Digests -> {
                // Latest server truth per session (bodies-free). Stored so an (re)attach
                // to a session whose hash still equals our cached digest skips the fetch.
                for (d in msg.items) serverDigest[d.name] = d.count to d.hash
            }
            is ServerMsg.HostList -> _hosts.value = msg.hosts
            is ServerMsg.IdentityList -> _identities.value = msg.identities
            is ServerMsg.Agents -> _agents.value = msg.agents
            is ServerMsg.Profiles -> _profiles.value = msg.profiles
            is ServerMsg.Err -> {
                // Version skew: an older server that predates the transcript-cache feature
                // rejects our connect-time `digest` probe with bad_message. That's harmless
                // (we just get no digests and fall back to fetching history), so swallow it
                // instead of spamming a scary note in the chat during a rollout.
                if (msg.code == "bad_message" && msg.message.contains("digest")) return
                if (msg.code == "turn_failed") { clearTurnInFlight(); streamedSessions.clear() }
                if (_usageLoading.value) _usageLoading.value = false // any error unsticks a pending usage fetch
                _activity.value = ""
                _mic.value = "" // a transcribe_failed / not_implemented error ends the PTT clip; clear "transcribing…"
                // Discover/adopt/delete errors surface on the Discover screen; the
                // rest go to the chat log.
                if (msg.code in setOf("session_active", "not_found", "bad_delete", "bad_adopt", "discover_failed")) {
                    _discoverError.value = msg.message
                } else {
                    addChat(Role.SYSTEM, "⚠️ ${msg.code}: ${msg.message}")
                }
            }
            is ServerMsg.TurnInterrupted -> {
                clearTurnInFlight()
                streamedSessions.remove(msg.name)
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.", key = msg.name)
                speakText("that turn got interrupted — the server restarted. say it again.")
            }
            is ServerMsg.TurnStopped -> {
                clearTurnInFlight()
                streamedSessions.remove(msg.name)
                _activity.value = ""
                cancelServerSpeech()
                speaker.stop() // also quiet any reply already being read
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                addChat(Role.SYSTEM, "⏹ stopped that turn.", key = msg.name)
            }
            is ServerMsg.Unknown -> {}
        }
    }

    // addChat appends a live message to the named session's log and reflects it
    // only when that session is the visible view. Historical messages come via
    // onHistory instead.
    private fun addChat(role: Role, text: String, usage: TokenUsage? = null, key: String = currentKey) {
        if (text.isBlank()) return
        val updated = ((logs[key] ?: emptyList()) + ChatMessage(role, text, usage = usage, ts = System.currentTimeMillis() / 1000)).takeLast(2000)
        logs[key] = updated
        if (key == currentKey) _chat.value = updated
        // A user/claude line grows this session's server transcript, so our stored
        // digest no longer matches — forget the server digest so the next reattach
        // refetches instead of wrongly deciding the cache is current. (SYSTEM notes
        // are live-only and never persisted server-side, so they don't invalidate.)
        if (role == Role.USER || role == Role.CLAUDE) serverDigest.remove(key)
    }

    // attachUsageToLastClaude badges the most recent Claude bubble in the named
    // log with a completed turn's token usage. Used when the reply streamed live
    // (the bubble was built from chunks, so the closing message can't add a new one).
    private fun attachUsageToLastClaude(key: String, usage: TokenUsage) {
        val log = logs[key] ?: return
        val idx = log.indexOfLast { it.role == Role.CLAUDE }
        if (idx < 0) return
        val updated = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
        logs[key] = updated
        if (key == currentKey) _chat.value = updated
    }

    /** Switch the visible chat to `key`'s log (session name, or "" for general). */
    private fun showLog(key: String) {
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
    private fun ordered(msgs: List<ChatMessage>): List<ChatMessage> {
        var carried = 0L
        val stamped = msgs.map { m ->
            if (m.ts > 0L) carried = m.ts
            m to carried
        }
        return stamped.sortedBy { it.second }.map { it.first }
    }

    // onHistory merges a server-served page of OLDER messages into the session's
    // log, ordered chronologically with any live messages, and updates the paging cursor.
    private fun onHistory(msg: ServerMsg.History) {
        // `unchanged` answers a top-page request whose have_hash still matched: our
        // cached transcript is current, so keep it untouched and just refresh the
        // stored digest (both held and server) so future freshness checks stand.
        if (msg.unchanged) {
            if (msg.hash.isNotEmpty()) {
                digestHeld[msg.name] = msg.count to msg.hash
                serverDigest[msg.name] = msg.count to msg.hash
            }
            loadingOlder.remove(msg.name)
            logs[msg.name]?.let { cleaned ->
                val deduped = dedupeCachedLog(cleaned)
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
        logs[msg.name] = dedupeCachedLog(ordered(hist + existing))
        if (msg.messages.isNotEmpty()) oldestIndex[msg.name] = msg.messages.first().index
        hasMore[msg.name] = msg.more
        loadingOlder.remove(msg.name)
        // Record the chain digest this page belongs to and persist the merged log, so
        // the cache is current on disk and a later reattach can short-circuit the fetch.
        if (msg.hash.isNotEmpty()) {
            digestHeld[msg.name] = msg.count to msg.hash
            serverDigest[msg.name] = msg.count to msg.hash
        }
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
    private fun onReadLast(count: Int) {
        val claude = _chat.value.filter { it.role == Role.CLAUDE }.takeLast(count.coerceAtLeast(1))
        if (claude.isEmpty()) {
            speakText("nothing to read yet")
        } else {
            speakText(claude.joinToString(". … ") { Markdown.toSpeech(it.text) })
        }
        _scrollTick.value = _scrollTick.value + 1
    }

    private fun roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE
}

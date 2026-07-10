package com.bam.spawner

import android.content.Context
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageReport
import com.bam.spawner.net.UsageEstimateInfo
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.audio.AudioRouter
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.audio.HandsFreeRecorder
import com.bam.spawner.audio.LevelMeter
import com.bam.spawner.audio.OpusRecorder
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SpawnerClient
import com.bam.spawner.tts.Markdown
import com.bam.spawner.tts.Speaker
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
    private val _discovered = MutableStateFlow<List<DiscoveredInfo>>(emptyList())
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

    // Pending clarification questions (interactive mode); null when none.
    private val _ask = MutableStateFlow<List<com.bam.spawner.net.AskQuestion>?>(null)
    override val ask: StateFlow<List<com.bam.spawner.net.AskQuestion>?> = _ask.asStateFlow()

    // AI backend registry from the `agents` message (backends + models for the picker).
    private val _agents = MutableStateFlow<List<com.bam.spawner.net.AgentInfo>>(emptyList())
    override val agents: StateFlow<List<com.bam.spawner.net.AgentInfo>> = _agents.asStateFlow()

    // Spoken-audio output routing (earpiece/speaker/bluetooth). `audioOutputs` is
    // what's currently selectable (bluetooth only when a headset is connected);
    // `audioOutput` is the active one.
    private val audioRouter = AudioRouter(app)
    private val _audioOutputs = MutableStateFlow(listOf(AudioOutput.EARPIECE, AudioOutput.SPEAKER))
    val audioOutputs: StateFlow<List<AudioOutput>> = _audioOutputs.asStateFlow()
    private val _audioOutput = MutableStateFlow(AudioOutput.EARPIECE)
    val audioOutput: StateFlow<AudioOutput> = _audioOutput.asStateFlow()

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
    // True once the current turn has streamed at least one live prose chunk
    // (output chunk=true). The final non-chunk output then just closes the turn
    // instead of re-adding/re-speaking text we already showed. Stays false for a
    // turn whose result arrives whole (a buffered reply on reconnect), so that
    // path still renders + speaks it.
    @Volatile private var turnStreamed = false

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
        // Restore the saved output (falling back to earpiece if it's no longer
        // available, e.g. the Bluetooth headset is off).
        val saved = runCatching { AudioOutput.valueOf(settings.audioOutput.uppercase()) }
            .getOrDefault(AudioOutput.EARPIECE)
        val available = audioRouter.available()
        _audioOutputs.value = available
        val target = if (saved in available) saved else AudioOutput.EARPIECE
        applyAudioOutput(target)
        _audioOutput.value = target
    }

    // Apply an output: MUTE suppresses TTS (no device routing); anything else
    // unmutes and routes the device. Returns whether it took effect.
    private fun applyAudioOutput(out: AudioOutput): Boolean =
        if (out == AudioOutput.MUTE) { speaker.setMuted(true); true }
        else { speaker.setMuted(false); audioRouter.setOutput(out) }

    /** Re-scan available outputs (call when opening the picker to catch a
     *  just-connected/removed Bluetooth headset). */
    fun refreshAudioOutputs() {
        val avail = audioRouter.available()
        _audioOutputs.value = avail
        // If the selected device vanished (e.g. the Bluetooth headset disconnected),
        // fall back to earpiece. MUTE is always available.
        val cur = _audioOutput.value
        if (cur != AudioOutput.MUTE && cur !in avail) setAudioOutput(AudioOutput.EARPIECE)
    }

    /** Route the spoken audio to [out] (or mute) and remember the choice. */
    fun setAudioOutput(out: AudioOutput) {
        if (applyAudioOutput(out)) {
            _audioOutput.value = out
            settings.audioOutput = out.name.lowercase()
        }
        _audioOutputs.value = audioRouter.available()
    }

    fun connect(url: String, token: String) {
        client?.close()
        _status.value = "connecting…"
        val hello = com.bam.spawner.net.HelloConfig(
            settings.endToken, settings.sttMode, settings.sttModel, settings.aliasMap(),
            settings.whisperUrl, settings.brief, settings.interactive,
            settings.autoCompress, settings.autoCompressThreshold,
        )
        // Present a client certificate when one is imported (mutual-TLS servers).
        // A bad passphrase / corrupt file is surfaced, then we fall back to a
        // cert-less connection (fine for plain ws:// or one-way wss://).
        val tls = if (settings.hasClientCert()) {
            try {
                com.bam.spawner.net.buildClientTls(settings.clientCertFile, settings.clientCertPass)
            } catch (e: Exception) {
                _status.value = "client cert error: ${e.message}"
                null
            }
        } else {
            null
        }
        client = SpawnerClient(url, token, settings.clientId, hello, ::onMessage, ::onConnected, tls)
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
        client?.send(Outbound.utterance(t))
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

    /** Adopt a discovered session into the registry and attach to it. */
    override fun adopt(sessionId: String, dir: String) = client?.send(Outbound.adopt(sessionId, dir)).let {}

    /** Permanently delete a discovered session's transcript from disk. */
    override fun deleteDiscovered(sessionId: String) = client?.send(Outbound.deleteDiscovered(sessionId)).let {}

    /** Give a discovered session a custom name (registers it by dir if needed). */
    override fun renameDiscovered(sessionId: String, dir: String, newName: String) =
        client?.send(Outbound.renameDiscovered(sessionId, dir, newName)).let {}

    override fun attachTo(name: String) {
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

    override fun detach() = client?.send(Outbound.detach()).let {}

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

    override fun spawnAt(path: String, target: String, host: String, agent: String, model: String) {
        client?.send(Outbound.spawnAt(path, target = target, host = host, agent = agent, model = model)) // the resulting `attached` switches the view
    }

    /** Create a new project folder `name` under `parent` and spawn a session in it. */
    override fun spawnNewFolder(parent: String, name: String, target: String, host: String, agent: String, model: String) {
        val clean = name.trim().trim('/')
        if (clean.isEmpty()) return
        client?.send(Outbound.spawnAt("$parent/$clean", create = true, target = target, host = host, agent = agent, model = model))
    }

    // --- Hands-free (always-listening VAD; only speech is sent) ---
    private var handsFree: HandsFreeRecorder? = null
    @Volatile private var hfOn = false


    private fun vadConfig() = com.bam.spawner.audio.VadConfig(
        rmsThreshold = settings.vadThreshold.toDouble(),
        onsetMs = settings.vadOnsetMs,
        silenceMs = settings.vadSilenceMs,
    )

    /** Starts the always-listening pipeline. Returns false if the mic is unavailable. */
    fun startHandsFree(): Boolean {
        if (hfOn) return true
        if (recording) return false // don't fight push-to-talk for the mic
        stopMeter() // free the mic if the level meter was running
        val hf = HandsFreeRecorder(app, vadConfig(), ::onHandsFreeSpeechStart, ::onHandsFreeUtterance) { _micLevel.value = it }
        if (!hf.start()) {
            _mic.value = "⚠️ mic unavailable"
            return false
        }
        handsFree = hf
        hfOn = true
        speaker.setCommMode(true) // echo-cancelled comm audio so voice barge-in works
        _voiceState.value = VoiceState.LISTENING
        _mic.value = "🟢 listening for \"hey buddy\"…"
        return true
    }

    /** Re-apply VAD settings live (restart the recorder) if hands-free is running. */
    fun restartHandsFree() {
        if (!hfOn) return
        handsFree?.stop()
        val hf = HandsFreeRecorder(app, vadConfig(), ::onHandsFreeSpeechStart, ::onHandsFreeUtterance) { _micLevel.value = it }
        if (hf.start()) {
            handsFree = hf
            _voiceState.value = VoiceState.LISTENING
        } else {
            handsFree = null; hfOn = false; _voiceState.value = VoiceState.OFF
        }
    }

    fun stopHandsFree() {
        hfOn = false
        speaker.setCommMode(false) // back to the regular media speaker
        handsFree?.stop()
        handsFree = null
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
    fun stopSpeaking() = speaker.stop()

    /** Change the resident whisper model (server-global; the server broadcasts the
     *  new value back to every client). */
    override fun setWhisperModel(model: String) = client?.send(Outbound.setWhisperModel(model)).let {}

    /** Push the auto-compress preference to the server (server-global; live). */
    override fun setAutoCompress(enabled: Boolean, thresholdK: Int) =
        client?.send(Outbound.autoCompress(enabled, thresholdK)).let {}

    /** Ask the server to restart. It exits so its supervisor relaunches it on
     *  current code; the app auto-reconnects once it's listening again. */
    override fun restartServer() = client?.send(Outbound.restart()).let {}

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
        c.send(Outbound.wake(HandsFreeRecorder.CODEC, handsFree = true))
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
        speaker.stop() // barge-in
        if (!recorder.start()) {
            _mic.value = "⚠️ mic unavailable"
            return
        }
        recording = true
        c.send(Outbound.wake(OpusRecorder.CODEC))
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
                speaker.speak("that turn may have been interrupted. try again if you don't hear back.")
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
                discover() // the drawer lists ALL machine sessions (discovery is the source)
                settings.lastSession.takeIf { it.isNotEmpty() }?.let {
                    // Prefer the stable id so we re-attach to the SAME session even when it's
                    // named differently on this server (e.g. after switching servers).
                    client?.send(Outbound.attach(it, sessionId = settings.lastSessionId, silent = true))
                }
            }
            is ServerMsg.WhisperModel -> {
                if (msg.model.isNotBlank()) { _whisperModel.value = msg.model; settings.whisperModel = msg.model }
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
                addChat(Role.SYSTEM, msg.text); speaker.speak(Markdown.toSpeech(msg.text))
            }
            is ServerMsg.Output -> {
                if (msg.chunk) {
                    // A live segment of Claude's reply as it's produced. Show + speak it
                    // now; a streamed chunk also proves the turn survived (like activity),
                    // so keep it in flight and disarm the interruption watchdog.
                    turnInFlight = true
                    lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
                    turnStreamed = true
                    _activity.value = "" // prose is arriving — drop the "thinking" breadcrumb
                    addChat(Role.CLAUDE, msg.text); speaker.speak(Markdown.toSpeech(msg.text))
                } else {
                    clearTurnInFlight()
                    _activity.value = "" // turn done — stop the thinking indicator
                    if (!turnStreamed) { // no live stream reached us (buffered reply on reconnect)
                        addChat(Role.CLAUDE, msg.text, msg.usage); speaker.speak(Markdown.toSpeech(msg.text))
                    } else if (msg.usage != null) {
                        // Streamed turn: the bubble already exists from chunks, so badge it
                        // in place — the closing message isn't re-rendered as a new bubble.
                        attachUsageToLastClaude(msg.usage)
                    }
                    turnStreamed = false
                    msg.usage?.let { _lastTurnUsage.value = TurnUsageInfo(it, nowMonotonicMs()) }
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
            is ServerMsg.Files -> if (msg.files.isNotEmpty()) addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "))
            is ServerMsg.Diff -> addChat(Role.SYSTEM, "📊 diff:\n${msg.text}") // review summary, not spoken
            is ServerMsg.RateLimit -> _rateLimit.value = msg.info // plan session-limit readout (sidebar)
            is ServerMsg.Usage -> { _usageLoading.value = false; _usageReport.value = msg.report } // opens the usage sheet
            is ServerMsg.UsageEstimate -> _usageEstimate.value = msg.est // drift-live footer/sheet estimate
            is ServerMsg.Ask -> {
                clearTurnInFlight()
                turnStreamed = false
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                _ask.value = msg.questions
                addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q })
                speaker.speak(spokenQuestions(msg.questions)) // read aloud so you can answer by voice
            }
            is ServerMsg.Transcript -> {
                _ask.value = null // a spoken/typed reply answers any pending questions
                turnStreamed = false // a new user turn begins; nothing streamed yet
                addChat(Role.USER, msg.text); _mic.value = ""
                if (hfOn) _voiceState.value = VoiceState.THINKING
            }
            is ServerMsg.Pending -> {
                _pending.value = msg.text
                if (msg.text.isEmpty()) cancelSilenceCommit() // committed/cleared
                if (hfOn) _voiceState.value = if (msg.text.isEmpty()) VoiceState.LISTENING else VoiceState.CAPTURING
            }
            is ServerMsg.Calibration -> onCalibrationSample(msg.text)
            is ServerMsg.StopSpeaking -> speaker.stop()
            is ServerMsg.Dialog -> _status.value = "dialog: ${msg.state}"
            is ServerMsg.Attached -> {
                // Fresh view of this session: drop any stale turn spinner/watchdog.
                // If a turn is genuinely still running, the server's bindJob sends a
                // "still working" breadcrumb right after this (which re-arms it); if
                // the turn finished while we were away, nothing comes and the spinner
                // correctly stays clear instead of hanging on "running the command".
                clearTurnInFlight()
                turnStreamed = false
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
                // Refetch recent history on EVERY (re)attach, not just the first. A
                // session keeps running while we view another one, and its output is
                // persisted to the transcript — but the server only fans live output to
                // the currently-attached connection, so anything it said while we were
                // away never reached us. Re-pulling the transcript on reattach replays
                // it so switching back to a busy session doesn't drop what it produced.
                // onHistory dedupes against live messages we already have.
                client?.send(Outbound.history(msg.name, null)) // load recent history
            }
            is ServerMsg.Detached -> {
                turnStreamed = false
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
            is ServerMsg.HostList -> _hosts.value = msg.hosts
            is ServerMsg.IdentityList -> _identities.value = msg.identities
            is ServerMsg.Agents -> _agents.value = msg.agents
            is ServerMsg.Err -> {
                if (msg.code == "turn_failed") { clearTurnInFlight(); turnStreamed = false }
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
                turnStreamed = false
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.")
                speaker.speak("that turn got interrupted — the server restarted. say it again.")
            }
            is ServerMsg.TurnStopped -> {
                clearTurnInFlight()
                turnStreamed = false
                _activity.value = ""
                speaker.stop() // also quiet any reply already being read
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                addChat(Role.SYSTEM, "⏹ stopped that turn.")
            }
            is ServerMsg.Unknown -> {}
        }
    }

    // addChat appends a live message to the CURRENT session's log (the view the
    // user is on) and reflects it. Historical messages come via onHistory instead.
    private fun addChat(role: Role, text: String, usage: TokenUsage? = null) {
        if (text.isBlank()) return
        val updated = ((logs[currentKey] ?: emptyList()) + ChatMessage(role, text, usage = usage, ts = System.currentTimeMillis() / 1000)).takeLast(2000)
        logs[currentKey] = updated
        _chat.value = updated
    }

    // attachUsageToLastClaude badges the most recent Claude bubble in the current
    // log with a completed turn's token usage. Used when the reply streamed live
    // (the bubble was built from chunks, so the closing message can't add a new one).
    private fun attachUsageToLastClaude(usage: TokenUsage) {
        val log = logs[currentKey] ?: return
        val idx = log.indexOfLast { it.role == Role.CLAUDE }
        if (idx < 0) return
        val updated = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
        logs[currentKey] = updated
        _chat.value = updated
    }

    /** Switch the visible chat to `key`'s log (session name, or "" for general). */
    private fun showLog(key: String) {
        currentKey = key
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
        logs[msg.name] = ordered(hist + existing)
        if (msg.messages.isNotEmpty()) oldestIndex[msg.name] = msg.messages.first().index
        hasMore[msg.name] = msg.more
        loadingOlder.remove(msg.name)
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
            speaker.speak("nothing to read yet")
        } else {
            speaker.speak(claude.joinToString(". … ") { Markdown.toSpeech(it.text) })
        }
        _scrollTick.value = _scrollTick.value + 1
    }

    private fun roleOf(role: String) = if (role == "user") Role.USER else Role.CLAUDE
}

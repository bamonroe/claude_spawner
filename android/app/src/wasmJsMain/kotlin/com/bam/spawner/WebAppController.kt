package com.bam.spawner

import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.AgentInfo
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.HelloConfig
import com.bam.spawner.net.Host
import com.bam.spawner.net.Identity
import com.bam.spawner.net.Outbound
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.SpawnerClient
import com.bam.spawner.net.UsageEstimateInfo
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
    private var turnStreamed = false

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
    private val _usageEstimate = MutableStateFlow<UsageEstimateInfo?>(null)
    override val usageEstimate: StateFlow<UsageEstimateInfo?> = _usageEstimate.asStateFlow()
    private val _usageReport = MutableStateFlow<UsageReport?>(null)
    override val usageReport: StateFlow<UsageReport?> = _usageReport.asStateFlow()
    private val _usageLoading = MutableStateFlow(false)
    override val usageLoading: StateFlow<Boolean> = _usageLoading.asStateFlow()

    private val _whisperModel = MutableStateFlow(prefs.whisperModel)
    override val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()
    // The fast (draft/detection, "quick") server's model; "" = none configured.
    private val _whisperFastModel = MutableStateFlow(prefs.whisperFastModel)
    override val whisperFastModel: StateFlow<String> = _whisperFastModel.asStateFlow()
    // Models available on the server's disk; not persisted — re-sent on connect.
    private val _whisperModels = MutableStateFlow<List<String>>(emptyList())
    override val whisperModels: StateFlow<List<String>> = _whisperModels.asStateFlow()
    private val _ask = MutableStateFlow<List<AskQuestion>?>(null)
    override val ask: StateFlow<List<AskQuestion>?> = _ask.asStateFlow()

    private val _agents = MutableStateFlow<List<AgentInfo>>(emptyList())
    override val agents: StateFlow<List<AgentInfo>> = _agents.asStateFlow()

    private val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    override val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()
    private val _fileSaved = MutableSharedFlow<String>(extraBufferCapacity = 4)
    override val fileSaved: SharedFlow<String> = _fileSaved.asSharedFlow()
    private val _fileData = MutableSharedFlow<ServerMsg.FileData>(extraBufferCapacity = 4)
    override val fileData: SharedFlow<ServerMsg.FileData> = _fileData.asSharedFlow()

    private val _hosts = MutableStateFlow<List<Host>>(emptyList())
    override val hosts: StateFlow<List<Host>> = _hosts.asStateFlow()
    private val _identities = MutableStateFlow<List<Identity>>(emptyList())
    override val identities: StateFlow<List<Identity>> = _identities.asStateFlow()

    /** (Re)connect to [url] with [token], sending the hello handshake built from prefs. */
    fun connect(url: String, token: String) {
        client?.close()
        _status.value = "connecting…"
        val hello = HelloConfig(
            prefs.endToken, prefs.wakeToken, prefs.speakToken, prefs.dictationGate,
            prefs.sttMode, prefs.sttModel, prefs.aliasMap(),
            prefs.whisperUrl, prefs.brief, prefs.interactive,
            prefs.warmCompress, prefs.autoCompress, prefs.autoCompressThreshold,
        )
        client = SpawnerClient(
            url = url, token = token, clientId = prefs.clientId, hello = hello,
            onMessage = ::onMessage,
            onConnected = { up -> _connected.value = up; if (!up) _status.value = "reconnecting…" },
        ).also { it.connect() }
    }

    private fun publish() {
        _chat.value = logs[currentKey] ?: emptyList()
        _hasMoreHistory.value = hasMore[currentKey] ?: false
    }

    private fun addChat(role: Role, text: String, usage: com.bam.spawner.net.TokenUsage? = null) {
        val now = nowEpochSeconds()
        logs[currentKey] = ((logs[currentKey] ?: emptyList()) +
            ChatMessage(role, text, usage = usage, ts = now)).takeLast(2000)
        if (currentKey.isNotEmpty() || role == Role.SYSTEM) publish()
        _scrollTick.value = _scrollTick.value + 1
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
                discover()
                if (prefs.lastSession.isNotBlank()) {
                    client?.send(Outbound.attach(prefs.lastSession, prefs.lastSessionId, silent = true))
                }
            }
            is ServerMsg.WhisperModel -> {
                if (msg.model.isNotBlank()) _whisperModel.value = msg.model
                _whisperFastModel.value = msg.fastModel
                prefs.whisperFastModel = msg.fastModel
                if (msg.models.isNotEmpty()) _whisperModels.value = msg.models
            }
            is ServerMsg.Say -> { _activity.value = ""; addChat(Role.SYSTEM, msg.text); speak(msg.text) }
            is ServerMsg.Output -> {
                _activity.value = ""
                // Summary-only: beep through intermediate steps, speak only the final result.
                val summaryOnly = prefs.summaryOnlySpeech
                if (msg.chunk) {
                    turnStreamed = true; addChat(Role.CLAUDE, msg.text)
                    if (summaryOnly) webBeep() else speak(msg.text)
                } else {
                    if (!turnStreamed) { addChat(Role.CLAUDE, msg.text, msg.usage); speak(msg.text) }
                    else {
                        if (msg.usage != null) attachUsageToLastClaude(msg.usage)
                        if (summaryOnly) speak(msg.text) // chunks only beeped — speak the final now
                    }
                    turnStreamed = false
                    // Anchor the cache-warm countdown to the turn's real completion
                    // time (usage_at), not to when a buffered reply reached us.
                    msg.usage?.let { u ->
                        val ageMs = if (msg.usageAt > 0) (nowEpochSeconds() - msg.usageAt) * 1000 else 0L
                        _lastTurnUsage.value = TurnUsageInfo(u, nowMonotonicMs() - ageMs.coerceIn(0, 6 * 60 * 1000L))
                    }
                }
            }
            is ServerMsg.StopSpeaking -> { cancelSpeech(); _speaking.value = false }
            is ServerMsg.SpeechMode -> prefs.summaryOnlySpeech = msg.summaryOnly // voice toggle mirrors the audio-settings switch
            is ServerMsg.ContextReset -> _lastTurnUsage.value = null
            is ServerMsg.Activity -> _activity.value = msg.text
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
            is ServerMsg.UsageEstimate -> _usageEstimate.value = msg.est
            is ServerMsg.Ask -> {
                _activity.value = ""; turnStreamed = false; _ask.value = msg.questions
                addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q })
            }
            is ServerMsg.Transcript -> { _ask.value = null; turnStreamed = false; addChat(Role.USER, msg.text) }
            is ServerMsg.Attached -> {
                turnStreamed = false; _activity.value = ""
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
                client?.send(Outbound.history(msg.name, null))
            }
            is ServerMsg.Detached -> {
                turnStreamed = false; _attachedId.value = ""; _attachedName.value = null
                _attachedAgent.value = ""; _attachedModel.value = ""
                prefs.lastSession = ""; prefs.lastSessionId = ""
                _status.value = "connected"; currentKey = ""; publish()
            }
            is ServerMsg.Renamed -> {
                if (msg.old == _attachedName.value || (msg.sessionId.isNotBlank() && msg.sessionId == _attachedId.value)) {
                    logs[msg.name] = logs.remove(msg.old) ?: emptyList()
                    if (currentKey == msg.old) currentKey = msg.name
                    _attachedName.value = msg.name; prefs.lastSession = msg.name
                    _status.value = "attached: ${msg.name}"
                    publish()
                }
            }
            is ServerMsg.History -> onHistory(msg)
            is ServerMsg.Discovered -> { _discovered.value = msg.sessions; _discoverError.value = "" }
            is ServerMsg.Listing -> _listing.value = msg
            is ServerMsg.FileSaved -> _fileSaved.tryEmit(msg.path)
            is ServerMsg.FileData -> _fileData.tryEmit(msg)
            is ServerMsg.HostList -> _hosts.value = msg.hosts
            is ServerMsg.IdentityList -> _identities.value = msg.identities
            is ServerMsg.Agents -> _agents.value = msg.agents
            is ServerMsg.Err -> {
                _activity.value = ""
                if (_usageLoading.value) _usageLoading.value = false
                if (msg.code in setOf("session_active", "not_found", "bad_delete", "bad_adopt", "discover_failed")) {
                    _discoverError.value = msg.message
                } else addChat(Role.SYSTEM, "⚠️ ${msg.code}: ${msg.message}")
            }
            is ServerMsg.TurnInterrupted -> { _activity.value = ""; turnStreamed = false; addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.") }
            is ServerMsg.TurnStopped -> { _activity.value = ""; turnStreamed = false; addChat(Role.SYSTEM, "⏹ stopped that turn.") }
            else -> {} // Pending / Calibration / ReadLast / Dialog / Unknown
        }
    }

    private fun attachUsageToLastClaude(usage: com.bam.spawner.net.TokenUsage) {
        val log = logs[currentKey] ?: return
        val idx = log.indexOfLast { it.role == Role.CLAUDE }
        if (idx < 0) return
        logs[currentKey] = log.toMutableList().also { it[idx] = it[idx].copy(usage = usage) }
        publish()
    }

    private fun onHistory(msg: ServerMsg.History) {
        val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index, usage = it.usage, ts = it.ts) }
        val existing = logs[msg.name] ?: emptyList()
        logs[msg.name] = if (loadingOlder) {
            // Prepend older page, keeping the live tail.
            (hist + existing.filter { it.index < 0 || it.index > (hist.lastOrNull()?.index ?: -1) })
                .distinctBy { if (it.index >= 0) "i${it.index}" else "l${it.text.hashCode()}" }
                .sortedBy { if (it.index >= 0) it.index else Int.MAX_VALUE }
        } else {
            hist
        }
        loadingOlder = false
        oldest[msg.name] = hist.firstOrNull()?.index ?: (oldest[msg.name] ?: 0)
        hasMore[msg.name] = msg.more
        if (msg.name == currentKey) { publish(); _scrollTick.value = _scrollTick.value + 1 }
    }

    // --- AppController methods -> Outbound sends ----------------------------
    override fun sendText(text: String) {
        val t = text.trim()
        if (t.isEmpty()) return
        addChat(Role.USER, t)
        client?.send(Outbound.utterance(t))
    }
    override fun attachTo(name: String) { client?.send(Outbound.attach(name)) }
    override fun detach() { client?.send(Outbound.detach()) }
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
    override fun spawnAt(path: String, target: String, host: String, agent: String, model: String) { client?.send(Outbound.spawnAt(path, target = target, host = host, agent = agent, model = model)) }
    override fun spawnNewFolder(parent: String, name: String, target: String, host: String, agent: String, model: String) {
        val clean = name.trim().trim('/')
        if (clean.isEmpty()) return
        client?.send(Outbound.spawnAt("$parent/$clean", create = true, target = target, host = host, agent = agent, model = model))
    }

    override fun browse(path: String, host: String, files: Boolean) { client?.send(Outbound.browse(path, host, files)) }
    override fun uploadFile(dir: String, name: String, contentB64: String, host: String) { client?.send(Outbound.upload(dir, name, contentB64, host)) }
    override fun downloadFile(path: String, host: String) { client?.send(Outbound.download(path, host)) }
    override fun attachedDirHost(): Pair<String, String>? =
        _discovered.value.firstOrNull { it.sessionId == _attachedId.value }?.let { it.dir to it.host }

    override fun requestHosts() { client?.send(Outbound.hostsList()) }
    override fun putHost(host: Host) { client?.send(Outbound.hostPut(host)) }
    override fun deleteHost(name: String) { client?.send(Outbound.hostDelete(name)) }
    override fun requestIdentities() { client?.send(Outbound.identitiesList()) }
    override fun createIdentity(name: String, user: String, password: String, genKey: Boolean) {
        client?.send(Outbound.identityCreate(name, user, password, genKey))
    }
    override fun importIdentity(name: String, user: String, password: String, keyPath: String) {
        client?.send(Outbound.identityImport(name, user, password, keyPath))
    }
    override fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) {
        client?.send(Outbound.identityUpdate(name, user, setPassword, password))
    }
    override fun deleteIdentity(name: String) { client?.send(Outbound.identityDelete(name)) }

    override fun requestUsage() { _usageLoading.value = true; _usageReport.value = null; client?.send(Outbound.usage()) }
    override fun setUsageBenchmark() { client?.send(Outbound.usageSet()) }
    override fun calcUsageMax() { client?.send(Outbound.usageCalc()) }
    override fun dismissUsage() { _usageLoading.value = false; _usageReport.value = null }

    override fun setWhisperModel(model: String, fast: Boolean) { client?.send(Outbound.setWhisperModel(model, fast)) }
    override fun setAutoCompress(warm: Boolean, auto: Boolean, thresholdK: Int) { client?.send(Outbound.autoCompress(warm, auto, thresholdK)) }
    override fun restartServer() { client?.send(Outbound.restart()) }

    // --- Push-to-talk mic capture (concrete, off the interface like Android) -------
    // Mirrors the phone: grab the mic on press, send the whole clip on release as
    // `pcm16` (wake → binary audio → audio_end). getUserMedia may prompt on first use;
    // if the button is released before permission resolves, stopMic returns "" and we
    // simply send nothing.

    /** Mic button pressed: barge-in over any speech, then start capturing. */
    fun startTalking() {
        if (capturing) return
        cancelSpeech(); _speaking.value = false // barge-in
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
        client?.send(Outbound.wake(Codecs.PCM16))
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
    fun stopSpeaking() { cancelSpeech(); _speaking.value = false }

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
                                speechActive() -> VoiceState.SPEAKING
                                handsFreeCapturing() -> VoiceState.CAPTURING
                                else -> VoiceState.LISTENING
                            }
                            val clip = pollHandsFreeClip().toString()
                            if (clip.isNotEmpty()) {
                                val pcm = Base64.decode(clip)
                                client?.send(Outbound.wake(Codecs.PCM16, handsFree = true))
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

    // Speak a reply through the browser (markdown stripped, same as the phone). Utterances
    // queue in the engine; a lightweight poll flips `speaking` off once the queue drains so
    // the SpeakingBar and its stop button track real playback.
    private fun speak(text: String) {
        if (_audioOutput.value == AudioOutput.MUTE) return
        val spoken = Markdown.toSpeech(text)
        if (spoken.isBlank()) return
        speakText(spoken)
        _speaking.value = true
        if (speakWatch?.isActive != true) {
            speakWatch = scope.launch {
                while (speechActive()) delay(250)
                _speaking.value = false
            }
        }
    }
}

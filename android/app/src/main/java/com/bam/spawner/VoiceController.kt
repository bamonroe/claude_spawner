package com.bam.spawner

import android.content.Context
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
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.launch

/** Who a chat message is from — drives left/right alignment in the UI. */
enum class Role { USER, CLAUDE, SYSTEM }

/** Hands-free pipeline state, surfaced as a status pill in the UI. */
enum class VoiceState { OFF, LISTENING, CAPTURING, THINKING, SPEAKING }

/** End-token calibration progress: how reliably the detection model hears the token. */
data class CalibrationState(
    val active: Boolean = false,
    val done: Boolean = false,
    val token: String = "",
    val rounds: Int = 10,
    val samples: List<String> = emptyList(), // what was heard each attempt
    val hits: Int = 0,
)

data class ChatMessage(val role: Role, val text: String, val index: Int = -1)

/**
 * Orchestrates the app's voice/chat loop: connects (with auto-reconnect),
 * streams push-to-talk audio, forwards typed utterances, keeps the session list
 * and per-session chat transcript, and reflects server messages into UI state +
 * text-to-speech.
 */
class VoiceController(context: Context, private val settings: SettingsStore) {
    private val app = context.applicationContext
    private val speaker = Speaker(app)
    private val recorder = OpusRecorder(app)
    private val notifier = Notifier(app)
    private var client: SpawnerClient? = null

    /** True while the app UI is in the foreground; drives whether a finished turn
     *  posts a notification. Set by the Activity's lifecycle. */
    @Volatile var appForeground = false

    private val _connected = MutableStateFlow(false)
    val connected: StateFlow<Boolean> = _connected.asStateFlow()

    private val _status = MutableStateFlow("disconnected")
    val status: StateFlow<String> = _status.asStateFlow()

    // Per-session chat logs, keyed by session name ("" = the general/unattached
    // view for dialog + system messages). `_chat` mirrors the current key's log.
    private val logs = mutableMapOf<String, List<ChatMessage>>()
    private val oldestIndex = mutableMapOf<String, Int>() // lowest history index held, per session
    private val hasMore = mutableMapOf<String, Boolean>() // older history remains to page, per session
    private val historyRequested = mutableSetOf<String>() // first history page requested, per session
    private val loadingOlder = mutableSetOf<String>()      // a page request is in flight, per session
    private var currentKey = ""

    private val _chat = MutableStateFlow<List<ChatMessage>>(emptyList())
    val chat: StateFlow<List<ChatMessage>> = _chat.asStateFlow()

    // Whether the current session has older history to page in (drives the
    // "load older" control), and a tick the UI watches to scroll to the bottom.
    private val _hasMoreHistory = MutableStateFlow(false)
    val hasMoreHistory: StateFlow<Boolean> = _hasMoreHistory.asStateFlow()
    private val _scrollTick = MutableStateFlow(0)
    val scrollTick: StateFlow<Int> = _scrollTick.asStateFlow()

    // Claude sessions found on disk (from `discover`) that can be adopted.
    private val _discovered = MutableStateFlow<List<DiscoveredInfo>>(emptyList())
    val discovered: StateFlow<List<DiscoveredInfo>> = _discovered.asStateFlow()

    // Last error from a discover/adopt/delete action, shown on the Discover
    // screen (otherwise it would go to the hidden chat log). "" = none.
    private val _discoverError = MutableStateFlow("")
    val discoverError: StateFlow<String> = _discoverError.asStateFlow()

    private val _attachedName = MutableStateFlow<String?>(null)
    val attachedName: StateFlow<String?> = _attachedName.asStateFlow()

    private val _listing = MutableStateFlow<ServerMsg.Listing?>(null)
    val listing: StateFlow<ServerMsg.Listing?> = _listing.asStateFlow()

    private val _mic = MutableStateFlow("")
    val mic: StateFlow<String> = _mic.asStateFlow()

    private val _voiceState = MutableStateFlow(VoiceState.OFF)
    val voiceState: StateFlow<VoiceState> = _voiceState.asStateFlow()

    // Live hands-free draft: what's captured but not yet committed (via end token).
    private val _pending = MutableStateFlow("")
    val pending: StateFlow<String> = _pending.asStateFlow()

    // Live mic RMS level (~0..32768) for the audio meter, fed by the running
    // hands-free recorder or a standalone LevelMeter.
    private val _micLevel = MutableStateFlow(0.0)
    val micLevel: StateFlow<Double> = _micLevel.asStateFlow()

    // Live "Claude is thinking / editing foo.go" indicator; "" when idle.
    private val _activity = MutableStateFlow("")
    val activity: StateFlow<String> = _activity.asStateFlow()

    // True while TTS is speaking (drives the tap-to-stop banner).
    private val _speaking = MutableStateFlow(false)
    val speaking: StateFlow<Boolean> = _speaking.asStateFlow()

    // The resident whisper model the SERVER currently has (server-global). Read on
    // connect; changed only via setWhisperModel. "" until the server reports it.
    private val _whisperModel = MutableStateFlow(settings.whisperModel)
    val whisperModel: StateFlow<String> = _whisperModel.asStateFlow()

    // Pending clarification questions (interactive mode); null when none.
    private val _ask = MutableStateFlow<List<com.bam.spawner.net.AskQuestion>?>(null)
    val ask: StateFlow<List<com.bam.spawner.net.AskQuestion>?> = _ask.asStateFlow()

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
        audioRouter.setOutput(target)
        _audioOutput.value = target
    }

    /** Re-scan available outputs (call when opening the picker to catch a
     *  just-connected/removed Bluetooth headset). */
    fun refreshAudioOutputs() {
        _audioOutputs.value = audioRouter.available()
        _audioOutput.value = audioRouter.current()
    }

    /** Route the spoken audio to [out] and remember the choice. */
    fun setAudioOutput(out: AudioOutput) {
        if (audioRouter.setOutput(out)) {
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
        )
        client = SpawnerClient(url, token, settings.clientId, hello, ::onMessage, ::onConnected)
            .also { it.connect() }
    }

    /** Connect only if we don't already have a client (survives Activity recreation). */
    fun connectIfNeeded(url: String, token: String) {
        if (client == null) connect(url, token)
    }

    fun sendText(text: String) {
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
    fun discover() = client?.send(Outbound.discover())

    /** Adopt a discovered session into the registry and attach to it. */
    fun adopt(sessionId: String, dir: String) = client?.send(Outbound.adopt(sessionId, dir))

    /** Permanently delete a discovered session's transcript from disk. */
    fun deleteDiscovered(sessionId: String) = client?.send(Outbound.deleteDiscovered(sessionId))

    /** Give a discovered session a custom name (registers it by dir if needed). */
    fun renameDiscovered(sessionId: String, dir: String, newName: String) =
        client?.send(Outbound.renameDiscovered(sessionId, dir, newName))

    fun attachTo(name: String) {
        showLog(name) // switch to that session's log immediately (cached if we have it)
        client?.send(Outbound.attach(name))
    }

    /** Load the previous page of older history for the current session. */
    fun loadOlder() {
        val key = currentKey
        if (key.isEmpty() || hasMore[key] != true || key in loadingOlder) return
        val before = oldestIndex[key] ?: return
        loadingOlder.add(key)
        client?.send(Outbound.history(key, before))
    }

    fun detach() = client?.send(Outbound.detach())

    /** Abort the running turn on the attached session (kills the claude child). */
    fun abortTurn() = client?.send(Outbound.abort())

    /** Submit answers to the pending interactive questions (from the dialog). The
     *  formatted text goes back as an ordinary turn — Claude has the questions in
     *  context via --resume. */
    fun submitAnswers(text: String) {
        _ask.value = null
        sendText(text)
    }

    /** Dismiss the questions without answering (they stay in the transcript). */
    fun dismissAsk() { _ask.value = null }

    private fun spokenQuestions(qs: List<AskQuestion>): String {
        fun opts(q: AskQuestion) = if (q.options.isEmpty()) "" else " Options: " + q.options.joinToString(", ") + "."
        if (qs.size == 1) return qs[0].q + opts(qs[0])
        return "I have ${qs.size} questions. " +
            qs.mapIndexed { i, q -> "${i + 1}: ${q.q}${opts(q)}" }.joinToString(" ")
    }

    // --- Visual directory browser (New session) ---
    fun browse(path: String) = client?.send(Outbound.browse(path))

    fun spawnAt(path: String) {
        client?.send(Outbound.spawnAt(path)) // the resulting `attached` switches the view
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
    }

    /** Stop the TTS readout (the on-screen tap-to-stop). */
    fun stopSpeaking() = speaker.stop()

    /** Change the resident whisper model (server-global; the server broadcasts the
     *  new value back to every client). */
    fun setWhisperModel(model: String) = client?.send(Outbound.setWhisperModel(model))

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
        when (msg) {
            is ServerMsg.HelloOk -> {
                _status.value = "connected"
                if (msg.whisperModel.isNotBlank()) { // adopt the server's current model
                    _whisperModel.value = msg.whisperModel
                    settings.whisperModel = msg.whisperModel
                }
                discover() // the drawer lists ALL machine sessions (discovery is the source)
                settings.lastSession.takeIf { it.isNotEmpty() }?.let {
                    client?.send(Outbound.attach(it, silent = true)) // reconnect: re-attach quietly
                }
            }
            is ServerMsg.WhisperModel -> {
                if (msg.model.isNotBlank()) { _whisperModel.value = msg.model; settings.whisperModel = msg.model }
            }
            is ServerMsg.Say -> { addChat(Role.SYSTEM, msg.text); speaker.speak(Markdown.toSpeech(msg.text)) }
            is ServerMsg.Output -> {
                clearTurnInFlight()
                _activity.value = "" // reply arrived — stop the thinking indicator
                addChat(Role.CLAUDE, msg.text); speaker.speak(Markdown.toSpeech(msg.text))
                if (!appForeground) notifier.turnDone(msg.name, msg.text) // surface it from the pocket
            }
            is ServerMsg.Activity -> {
                // A live breadcrumb means the turn is running server-side; mark it in
                // flight and disarm any interruption watchdog (it survived a reconnect).
                turnInFlight = true
                lostTurnWatchdog?.cancel(); lostTurnWatchdog = null
                _activity.value = msg.text
            }
            is ServerMsg.Files -> if (msg.files.isNotEmpty()) addChat(Role.SYSTEM, "📝 changed: " + msg.files.joinToString(", "))
            is ServerMsg.Diff -> addChat(Role.SYSTEM, "📊 diff:\n${msg.text}") // review summary, not spoken
            is ServerMsg.Ask -> {
                clearTurnInFlight()
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                _ask.value = msg.questions
                addChat(Role.SYSTEM, "❓ " + msg.questions.joinToString("  ") { it.q })
                speaker.speak(spokenQuestions(msg.questions)) // read aloud so you can answer by voice
            }
            is ServerMsg.Transcript -> {
                _ask.value = null // a spoken/typed reply answers any pending questions
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
                _activity.value = ""
                _attachedName.value = msg.name
                settings.lastSession = msg.name
                _status.value = "attached: ${msg.name}"
                showLog(msg.name)
                if (msg.name !in historyRequested) {
                    historyRequested.add(msg.name)
                    client?.send(Outbound.history(msg.name, null)) // load recent history
                }
            }
            is ServerMsg.Detached -> {
                _attachedName.value = null
                settings.lastSession = ""
                _status.value = "connected"
                showLog("")
            }
            is ServerMsg.History -> onHistory(msg)
            is ServerMsg.ReadLast -> onReadLast(msg.count)
            is ServerMsg.Discovered -> { _discovered.value = msg.sessions; _discoverError.value = "" }
            is ServerMsg.Listing -> _listing.value = msg
            is ServerMsg.Err -> {
                if (msg.code == "turn_failed") clearTurnInFlight()
                _activity.value = ""
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
                _activity.value = ""
                if (hfOn) _voiceState.value = VoiceState.LISTENING
                addChat(Role.SYSTEM, "⚠️ turn interrupted (${msg.reason}) — say it again.")
                speaker.speak("that turn got interrupted — the server restarted. say it again.")
            }
            is ServerMsg.TurnStopped -> {
                clearTurnInFlight()
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
    private fun addChat(role: Role, text: String) {
        if (text.isBlank()) return
        val updated = ((logs[currentKey] ?: emptyList()) + ChatMessage(role, text)).takeLast(2000)
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

    // onHistory merges a server-served page of OLDER messages into the session's
    // log (prepended, ahead of any live messages), and updates the paging cursor.
    private fun onHistory(msg: ServerMsg.History) {
        val wasLoadOlder = msg.name in loadingOlder // else it's the initial page (on attach)
        val hist = msg.messages.map { ChatMessage(roleOf(it.role), it.text, it.index) }
        val histIdx = hist.mapNotNull { if (it.index >= 0) it.index else null }.toSet()
        // keep live messages (index < 0) and any already-loaded page not in this one
        val existing = (logs[msg.name] ?: emptyList()).filter { it.index < 0 || it.index !in histIdx }
        logs[msg.name] = hist + existing
        if (msg.messages.isNotEmpty()) oldestIndex[msg.name] = msg.messages.first().index
        hasMore[msg.name] = msg.more
        loadingOlder.remove(msg.name)
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

package com.bam.spawner

import com.bam.spawner.net.AgentInfo
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.UsageEstimateInfo
import com.bam.spawner.net.UsageReport
import kotlinx.coroutines.flow.SharedFlow
import kotlinx.coroutines.flow.StateFlow

/** A per-turn token badge tagged with the elapsed-time it landed at (drives the cache-warm timer). */
data class TurnUsageInfo(val usage: TokenUsage, val atElapsedMs: Long)

/**
 * Live progress of an on-demand whisper model download (the server fetches a catalogue model
 * the operator hasn't placed on disk when a client selects it). [total] 0 = unknown size;
 * [done] with a non-blank [error] means it failed. Null when no download is in flight.
 */
data class WhisperDownloadInfo(
    val model: String, val fast: Boolean, val received: Long, val total: Long,
    val done: Boolean, val error: String,
)

/**
 * The slice of the app the shared, pure-Compose UI reads and drives. The Android
 * [VoiceController] implements it, and a future web controller will too, so the chat,
 * sidebar, browse, and settings screens can live in `commonMain` and render identically
 * on both clients.
 *
 * This is deliberately the *shared* surface only: everything here is expressed in terms of
 * `commonMain` types (chat models, `net` protocol types) — the Android-only concerns (mic
 * capture, wake word, TTS, audio routing, the client-cert file, connect/lifecycle) stay on
 * the concrete class and are driven by the platform, not the shared UI. State that the UI
 * merely *reads* but that a platform *fills in* (e.g. [voiceState], [speaking], [activity])
 * lives here as read-only flows; a web controller stubs them until M5 wires browser audio.
 *
 * Extends [HostsIdentitiesController] (which already contributes `connected`/`hosts`/
 * `identities` and their editing methods) so a single interface covers the whole UI.
 */
interface AppController : HostsIdentitiesController, ProfilesController, ProvidersController {
    // --- Connection / status -------------------------------------------------
    val status: StateFlow<String>

    // --- Chat log ------------------------------------------------------------
    val chat: StateFlow<List<ChatMessage>>
    val hasMoreHistory: StateFlow<Boolean>
    val scrollTick: StateFlow<Int>
    val pending: StateFlow<String>

    // --- Attach / discovery --------------------------------------------------
    val attachedName: StateFlow<String?>
    val attachedId: StateFlow<String>
    // The attached session's AI backend id and model alias (from `attached`), for
    // the status-bar badge. Empty when detached or on a pre-agent server.
    val attachedAgent: StateFlow<String>
    val attachedModel: StateFlow<String>
    val discovered: StateFlow<List<DiscoveredInfo>>
    val discoverError: StateFlow<String>

    // --- Hands-free / voice pipeline (platform-filled; web stubs) ------------
    val voiceState: StateFlow<VoiceState>
    val speaking: StateFlow<Boolean>
    val activity: StateFlow<String>

    // --- Usage / rate limits -------------------------------------------------
    val lastTurnUsage: StateFlow<TurnUsageInfo?>
    val rateLimit: StateFlow<RateLimitInfo?>
    val usageEstimate: StateFlow<UsageEstimateInfo?>
    val usageReport: StateFlow<UsageReport?>
    val usageLoading: StateFlow<Boolean>

    // --- Misc UI state -------------------------------------------------------
    val whisperModel: StateFlow<String>
    // The fast (draft/detection, "quick" transcribe) server's model; "" when the
    // server has no fast whisper server configured.
    val whisperFastModel: StateFlow<String>
    // The English-model catalogue the settings picker offers (plus any extra
    // on-disk ggml file); empty when the server doesn't advertise it → free text.
    val whisperModels: StateFlow<List<String>>
    // The subset of whisperModels already downloaded on the server; a catalogue
    // entry not in this list is fetched on select.
    val whisperModelsLocal: StateFlow<List<String>>
    // Live progress of an on-demand model download; null when none is in flight.
    val whisperDownload: StateFlow<WhisperDownloadInfo?>
    // Whether the connected server offers Kokoro speech synthesis (hello_ok
    // `tts`) — the audio-settings "Server voice" toggle only takes effect then.
    val serverTtsAvailable: StateFlow<Boolean>
    // Kokoro's voice catalogue + the server-default voice (from the tts_voices
    // reply; both empty until the server offers TTS) — feeds the voice picker.
    val ttsVoices: StateFlow<List<String>>
    val ttsVoiceDefault: StateFlow<String>
    val ask: StateFlow<List<AskQuestion>?>
    // AI backend registry (from the `agents` message): the backends + models the
    // new-session picker offers. Empty until the server advertises it on connect.
    val agents: StateFlow<List<AgentInfo>>
    // Execution profiles (`profiles` message) come from [ProfilesController], shared
    // with the Settings → Profiles editor; the new-session picker reads them too.

    // --- File browse / transfer ----------------------------------------------
    val listing: StateFlow<ServerMsg.Listing?>
    val fileSaved: SharedFlow<String>
    val fileData: SharedFlow<ServerMsg.FileData>

    // --- Turn I/O ------------------------------------------------------------
    fun sendText(text: String)
    fun attachTo(name: String)
    fun detach()
    fun abortTurn()
    fun loadOlder()
    fun submitAnswers(text: String)
    fun dismissAsk()

    // --- Discovery / spawn ---------------------------------------------------
    fun discover()
    fun adopt(sessionId: String, dir: String)
    fun deleteDiscovered(sessionId: String)
    fun renameDiscovered(sessionId: String, dir: String, newName: String)
    fun setAgent(sessionId: String, dir: String, agent: String, model: String)
    fun spawnAt(path: String, target: String = "", host: String = "", agent: String = "", model: String = "", profile: String = "")
    fun spawnNewFolder(parent: String, name: String, target: String = "", host: String = "", agent: String = "", model: String = "", profile: String = "")

    // --- Browse / file transfer ----------------------------------------------
    fun browse(path: String, host: String = "", files: Boolean = false)
    fun uploadFile(dir: String, name: String, contentB64: String, host: String = "")
    fun downloadFile(path: String, host: String = "")
    fun attachedDirHost(): Pair<String, String>?

    // --- Usage ---------------------------------------------------------------
    fun requestUsage()
    fun setUsageBenchmark()
    fun calcUsageMax()
    fun dismissUsage()

    // --- Server controls -----------------------------------------------------
    /** Hot-load a resident whisper model: the accurate ("full") server, or the fast ("quick") one. */
    fun setWhisperModel(model: String, fast: Boolean = false)
    // Speak a short sample in [voice] through the server (the voice picker's
    // preview; no-op when server TTS is off or unavailable).
    fun previewTtsVoice(voice: String)
    /** Push the context-compression preference (warm + auto) to the server (live, no reconnect). */
    fun setAutoCompress(warm: Boolean, auto: Boolean, thresholdK: Int)
    /** Restart the server; rebuild=true recompiles from source, false is a fast bounce that reuses the current image. */
    fun restartServer(rebuild: Boolean = true)
}

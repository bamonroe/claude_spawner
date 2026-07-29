package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Slider
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.flow.StateFlow

/**
 * Audio settings: VAD thresholds, TTS toggles, the resident-whisper URL, and the
 * server-global transcription model pair (full + quick) — prefs backed by [Prefs], the models
 * read/pushed live via [controller]. (The end token, wake token, and silence auto-commit live on
 * the Commands page, since they're part of the command grammar; the "brief replies" and "ask
 * before guessing" session-behavior toggles live on the Server page.) [micMeter] is a platform slot
 * that draws the live mic-level bar (Android reads the recorder; web is empty until M5's Web
 * Audio); it receives the current threshold so it can mark it on the bar.
 */
@Composable
fun AudioSettings(
    settings: Prefs,
    controller: AppController,
    onVadChanged: () -> Unit,
    onSttChanged: () -> Unit,
    onBack: () -> Unit,
    micMeter: @Composable (Double) -> Unit = {},
) {
    var threshold by remember { mutableStateOf(settings.vadThreshold.toFloat()) }

    SettingsScaffold("Audio", onBack) {
        Text("Hands-free listening", style = MaterialTheme.typography.titleMedium)
        Text(
            "These dials decide when the phone thinks you're talking. Nothing is sent until a "
                + "sound clears the mic threshold, holds for the start time, and is followed by "
                + "silence — so tuning them is how you fight false triggers in a noisy place versus "
                + "missed words. In steady background noise, turn on \"Adapt\" below.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        micMeter(threshold.toDouble())
        Text("Mic threshold: ${threshold.toInt()}", style = MaterialTheme.typography.bodyMedium)
        Text("How loud a sound must be to count as speech. Lower catches quiet talk but triggers on more noise; higher needs you louder. With \"Adapt\" on, this is just the floor.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        Slider(
            value = threshold, onValueChange = { threshold = it },
            valueRange = 200f..1500f, steps = 12,
            onValueChangeFinished = { settings.vadThreshold = threshold.toInt(); onVadChanged() },
        )
        VadSlider("Speech to start (ms)", settings.vadOnsetMs, 40, 400, 20,
            "How long a sound must stay above the bar before capture begins. Longer rejects brief clicks, taps and blips; too long clips your first word.") {
            settings.vadOnsetMs = it; onVadChanged()
        }
        VadSlider("Silence to end (ms)", settings.vadSilenceMs, 400, 2000, 100,
            "How long you go quiet before the phrase is treated as finished and sent.") {
            settings.vadSilenceMs = it; onVadChanged()
        }
        VadFloatSlider("Max phrase length (s)", settings.vadMaxSeconds, 5f, 30f, 1f,
            "Safety cap — capture ends this long after you start even if silence is never heard. In constant wind, road or engine noise that never dips, this is what stops one clip from running on forever.") {
            settings.vadMaxSeconds = it; onVadChanged()
        }
        var adaptive by remember { mutableStateOf(settings.vadAdaptive) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Adapt to background noise", style = MaterialTheme.typography.titleMedium)
                Text("Continuously measure the room's noise floor and lift the mic threshold above it, so steady noise (a train, a fan, road hum) doesn't keep tripping the mic. The threshold slider above then acts as a minimum. Android only.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = adaptive, onCheckedChange = { adaptive = it; settings.vadAdaptive = it; onVadChanged() })
        }
        if (adaptive) {
            VadFloatSlider("Noise margin (×)", settings.vadNoiseRatio, 1.5f, 5f, 0.1f,
                "When adapting, how far above the measured noise floor your voice must rise. Higher rejects louder background noise but demands you speak up; lower is more sensitive but lets steady noise leak through.") {
                settings.vadNoiseRatio = it; onVadChanged()
            }
        }
        var headsetNs by remember { mutableStateOf(settings.headsetNoiseSuppression) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Headset noise suppression", style = MaterialTheme.typography.titleMedium)
                Text("Run the phone's noise suppressor on the headset mic path too. Helps steady background noise, but can attenuate far-field voice.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = headsetNs, onCheckedChange = { headsetNs = it; settings.headsetNoiseSuppression = it; onVadChanged() })
        }

        // The hands-free mic source (device vs headset) now lives in the top-bar audio
        // picker's Input section, alongside the output route, so it isn't set here.

        // Server-side denoise: the heavyweight DeepFilterNet scrub runs on the server
        // (needs SPAWNER_DENOISE_URL); it only helps clips that already made it past
        // the on-device gate above, so it's the second half of the noise story. Rides
        // the hello handshake, so a change reconnects via onSttChanged.
        val denoiseAvailable by controller.serverDenoiseAvailable.collectAsState()
        var denoise by remember { mutableStateOf(settings.denoiseEnabled) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Server-side denoise", style = MaterialTheme.typography.titleMedium)
                Text(
                    if (denoiseAvailable)
                        "Run each captured clip through the server's DeepFilterNet noise remover "
                            + "before it's transcribed — a much stronger scrub of steady wind, road, "
                            + "engine and fan noise than the phone can do. It adds a little latency to "
                            + "every phrase, so leave it off in quiet places."
                    else
                        "This server has no denoiser configured (SPAWNER_DENOISE_URL) — clips are "
                            + "transcribed as-is.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                )
            }
            Switch(
                checked = denoise, enabled = denoiseAvailable,
                onCheckedChange = { denoise = it; settings.denoiseEnabled = it; onSttChanged() },
            )
        }
        if (denoiseAvailable && denoise) {
            VadFloatSlider("Noise removed (dB)", settings.denoiseAttenDb, 6f, 48f, 1f,
                "How much steady noise the scrubber may subtract. Higher strips more (cleaner in "
                    + "heavy wind or road noise); lower is gentler and keeps the voice more natural. "
                    + "48 is effectively full strength.") {
                settings.denoiseAttenDb = it; onSttChanged()
            }
        }

        HorizontalDivider()
        var summaryOnly by remember { mutableStateOf(settings.summaryOnlySpeech) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Summary only", style = MaterialTheme.typography.titleMedium)
                Text("Only speak a turn's final result; intermediate steps play a soft beep instead. Say \"hey buddy, summary only\" / \"speak everything\" to toggle by voice.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = summaryOnly, onCheckedChange = { summaryOnly = it; controller.setSummaryOnly(it) })
        }
        if (summaryOnly) {
            var speakInitial by remember { mutableStateOf(settings.speakInitialReplies.toString()) }
            OutlinedTextField(
                value = speakInitial,
                onValueChange = { v ->
                    speakInitial = v.filter { it.isDigit() }.take(2)
                    settings.speakInitialReplies = speakInitial.toIntOrNull()?.coerceAtLeast(0) ?: 0
                },
                label = { Text("Speak initial replies") },
                supportingText = { Text("In summary-only mode, speak the first N replies of each turn aloud (the rest beep); the final summary is always spoken. 0 = summary only.") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                modifier = Modifier.fillMaxWidth(),
            )
        }
        var serverTts by remember { mutableStateOf(settings.serverTts) }
        val ttsAvailable by controller.serverTtsAvailable.collectAsState()
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Server voice", style = MaterialTheme.typography.titleMedium)
                Text(
                    if (ttsAvailable)
                        "Speak with the server's Kokoro voice, streamed to this device. Off (or on any failure) the device's own voice is used."
                    else
                        "This server doesn't offer speech synthesis — the device's own voice is used.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                )
            }
            Switch(
                checked = serverTts, enabled = ttsAvailable,
                onCheckedChange = { serverTts = it; settings.serverTts = it },
            )
        }
        // The Kokoro voice picker (server-relayed catalogue; empty until the server
        // offers TTS). Client-local: the choice rides each speak request; picking a
        // voice speaks a short preview in it.
        val ttsVoices by controller.ttsVoices.collectAsState()
        val ttsVoiceDefault by controller.ttsVoiceDefault.collectAsState()
        if (ttsAvailable && ttsVoices.isNotEmpty()) {
            var voice by remember { mutableStateOf(settings.ttsVoice) }
            var voicesOpen by remember { mutableStateOf(false) }
            Box {
                OutlinedButton(onClick = { voicesOpen = true }, modifier = Modifier.fillMaxWidth()) {
                    Text("Voice: ${voice.ifBlank { "server default ($ttsVoiceDefault)" }} ▾")
                }
                DropdownMenu(expanded = voicesOpen, onDismissRequest = { voicesOpen = false }) {
                    DropdownMenuItem(
                        text = { Text("server default ($ttsVoiceDefault)") },
                        onClick = {
                            voice = ""; settings.ttsVoice = ""; voicesOpen = false
                            controller.previewTtsVoice("")
                        },
                    )
                    ttsVoices.forEach { v ->
                        DropdownMenuItem(text = { Text(v) }, onClick = {
                            voice = v; settings.ttsVoice = v; voicesOpen = false
                            controller.previewTtsVoice(v) // hear it right away
                        })
                    }
                }
            }
        }

        HorizontalDivider()
        Text("Transcription models", style = MaterialTheme.typography.titleMedium)
        Text(
            "Server-global — shared by every device, hot-loaded on the whisper containers (a few "
                + "seconds). Pick any English model; one marked ⤓ isn't on the server yet and downloads "
                + "on apply (a big model is a slow first fetch, then it's cached).",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        WhisperModelField(
            "Full transcribe model", controller.whisperModel,
            controller.whisperModels, controller.whisperModelsLocal,
        ) { controller.setWhisperModel(it) }
        WhisperModelField(
            "Quick transcribe model", controller.whisperFastModel,
            controller.whisperModels, controller.whisperModelsLocal,
        ) { controller.setWhisperModel(it, fast = true) }
        WhisperDownloadBanner(controller.whisperDownload)
        Text(
            "Full handles dictation; quick handles the live hands-free draft and end-token "
                + "detection. Quick shows \"none\" when the server has no fast whisper server.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
    }
}

/**
 * One server-global whisper model editor, prefilled from the server-reported [current]
 * (re-synced on any change, even from another device), with an Apply that pushes it via
 * [onApply]. When the server advertises its on-disk models ([options] non-empty) the editor
 * is a dropdown of them; otherwise it falls back to a free-text ggml model name. Re-applying
 * the unchanged name is a deliberate pin: the server skips the redundant hot-load but persists
 * the choice to settings.json, so a model that only came from an env default survives restarts.
 */
@Composable
private fun WhisperModelField(
    label: String,
    current: StateFlow<String>,
    options: StateFlow<List<String>>,
    localOptions: StateFlow<List<String>>,
    onApply: (String) -> Unit,
) {
    val cur by current.collectAsState()
    val models by options.collectAsState()
    val local by localOptions.collectAsState()
    var picked by remember { mutableStateOf(cur) }
    LaunchedEffect(cur) { picked = cur }
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        if (models.isNotEmpty()) {
            var open by remember { mutableStateOf(false) }
            // A catalogue model not yet on the server's disk shows a ⤓ so the user knows
            // applying it triggers a download rather than an instant hot-load.
            val needsDownload = picked.isNotBlank() && picked !in local
            Box(Modifier.weight(1f)) {
                OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) {
                    Text("$label: ${picked.ifBlank { "none" }}${if (needsDownload) " ⤓" else ""} ▾")
                }
                DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
                    models.forEach { m ->
                        val label = if (m in local) m else "$m  ⤓ download"
                        DropdownMenuItem(text = { Text(label) }, onClick = { picked = m; open = false })
                    }
                }
            }
        } else {
            OutlinedTextField(
                picked, { picked = it },
                label = { Text(label) }, singleLine = true, modifier = Modifier.weight(1f),
                placeholder = { Text(cur.ifBlank { "none" }) },
            )
        }
        OutlinedButton(
            onClick = { onApply(picked.trim()) },
            enabled = picked.trim().isNotBlank(),
        ) { Text("Apply") }
    }
    Text(
        "current: ${cur.ifBlank { "none" }}",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
}

/**
 * Live banner for an on-demand model download the server runs when a client picks a model that
 * isn't on disk yet. Shows a determinate bar when the size is known, an indeterminate one
 * otherwise, and the error (kept visible) on failure. Renders nothing when no download is active.
 */
@Composable
private fun WhisperDownloadBanner(download: StateFlow<WhisperDownloadInfo?>) {
    val d by download.collectAsState()
    val dl = d ?: return
    Column(Modifier.fillMaxWidth()) {
        if (dl.error.isNotBlank()) {
            Text(
                "Download of ${dl.model} failed: ${dl.error}",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error,
            )
            return
        }
        val detail = if (dl.total > 0) "${mbTenths(dl.received)} / ${mbTenths(dl.total)} MB"
        else "${mbTenths(dl.received)} MB"
        Text(
            "Downloading ${dl.model} — $detail",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (dl.total > 0) {
            LinearProgressIndicator(
                progress = { (dl.received.toFloat() / dl.total.toFloat()).coerceIn(0f, 1f) },
                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
            )
        } else {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth().padding(top = 4.dp))
        }
    }
}

/** Formats a byte count as megabytes with one decimal, without String.format (KMP-safe). */
private fun mbTenths(bytes: Long): String {
    val tenths = bytes * 10 / 1_000_000
    return "${tenths / 10}.${tenths % 10}"
}

/** A labeled slider for an integer VAD dial, with an optional one-line explanation;
 *  persists on release via [onChange]. */
@Composable
fun VadSlider(label: String, initial: Int, min: Int, max: Int, step: Int, help: String = "", onChange: (Int) -> Unit) {
    var v by remember { mutableStateOf(initial.toFloat()) }
    val steps = ((max - min) / step - 1).coerceAtLeast(0)
    Column {
        Text("$label: ${v.toInt()}", style = MaterialTheme.typography.bodyMedium)
        if (help.isNotBlank())
            Text(help, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        Slider(
            value = v,
            onValueChange = { v = it },
            valueRange = min.toFloat()..max.toFloat(),
            steps = steps,
            onValueChangeFinished = { onChange(v.toInt()) },
        )
    }
}

/** A labeled slider for a one-decimal VAD dial (e.g. a ratio or a duration in seconds),
 *  with an optional explanation; persists on release via [onChange]. */
@Composable
fun VadFloatSlider(label: String, initial: Float, min: Float, max: Float, step: Float, help: String = "", onChange: (Float) -> Unit) {
    var v by remember { mutableStateOf(initial) }
    val steps = (((max - min) / step).toInt() - 1).coerceAtLeast(0)
    // Snap to the step grid and round to one decimal so persisted values stay clean.
    fun snap(x: Float): Float {
        val snapped = min + kotlin.math.round((x - min) / step) * step
        return kotlin.math.round(snapped * 10f) / 10f
    }
    Column {
        Text("$label: ${oneDecimal(v)}", style = MaterialTheme.typography.bodyMedium)
        if (help.isNotBlank())
            Text(help, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        Slider(
            value = v,
            onValueChange = { v = it },
            valueRange = min..max,
            steps = steps,
            onValueChangeFinished = { onChange(snap(v)) },
        )
    }
}

/** Formats a float with one decimal without String.format (KMP-safe). */
private fun oneDecimal(v: Float): String {
    val tenths = kotlin.math.round(v * 10f).toInt()
    return "${tenths / 10}.${tenths % 10}"
}

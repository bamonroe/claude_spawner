@file:OptIn(ExperimentalLayoutApi::class)

package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.tts.TtsCatalogue
import com.bam.spawner.tts.TtsEngine
import com.bam.spawner.tts.TtsModel
import com.bam.spawner.tts.TtsModelState

/**
 * Settings › Audio › Text to speech: pick which of the four engines speaks, and
 * manage the models the on-device ones need.
 *
 * The section is deliberately a **bench**: all four engines stay one tap apart so
 * they can be compared on the same reply, rather than one being chosen once and
 * buried. Whatever is selected, the device's own voice remains the fallback, so
 * no choice here can leave the app silent.
 */
@Composable
fun TtsEngineSection(settings: Prefs, controller: AppController) {
    var engine by remember { mutableStateOf(settings.ttsEngine) }
    val serverAvailable by controller.serverTtsAvailable.collectAsState()
    val models by controller.localTtsModels.collectAsState()

    Text("Text to speech", style = MaterialTheme.typography.titleMedium)
    Text(
        "Which engine reads replies aloud. The device's own voice is always the "
            + "fallback — if the server is down or a downloaded model is missing, speech "
            + "still happens, just in the Android voice.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )

    // Four pills don't fit one phone-width line, so they wrap rather than the last
    // engine falling off the right edge where it can't be tapped.
    FlowRow(
        horizontalArrangement = Arrangement.spacedBy(8.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp),
    ) {
        TtsEngine.ALL.forEach { e ->
            // The browser can't run sherpa-onnx; hide the local engines there
            // rather than offering a choice that can't take effect.
            if (TtsEngine.isLocal(e) && !controller.localTtsSupported) return@forEach
            ThemeChoice(TtsEngine.label(e), engine == e) {
                engine = e
                settings.ttsEngine = e
            }
        }
    }

    when (engine) {
        TtsEngine.SERVER -> ServerVoicePicker(settings, controller, serverAvailable)
        TtsEngine.ANDROID -> Text(
            "Android's built-in speech engine. Nothing to download and the fastest to "
                + "start speaking, but the voice is whatever the system provides.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        TtsEngine.KOKORO -> LocalKokoro(settings, controller, models)
        TtsEngine.PIPER -> LocalPiper(settings, controller, models)
    }
}

/** The server's Kokoro catalogue (relayed by `tts_voices`; empty until it offers TTS). */
@Composable
private fun ServerVoicePicker(settings: Prefs, controller: AppController, available: Boolean) {
    val voices by controller.ttsVoices.collectAsState()
    val default by controller.ttsVoiceDefault.collectAsState()
    Text(
        if (available)
            "Kokoro synthesized on the server and streamed here as audio. No download, "
                + "and the server does the work — but it needs a live connection."
        else
            "This server doesn't offer speech synthesis, so the device's own voice is "
                + "used until it does.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
    if (!available || voices.isEmpty()) return
    var voice by remember { mutableStateOf(settings.ttsVoice) }
    var open by remember { mutableStateOf(false) }
    Box {
        OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) {
            Text("Voice: ${voice.ifBlank { "server default ($default)" }} ▾")
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            DropdownMenuItem(
                text = { Text("server default ($default)") },
                onClick = {
                    voice = ""; settings.ttsVoice = ""; open = false
                    controller.previewTtsVoice("")
                },
            )
            voices.forEach { v ->
                DropdownMenuItem(text = { Text(v) }, onClick = {
                    voice = v; settings.ttsVoice = v; open = false
                    controller.previewTtsVoice(v) // hear it right away
                })
            }
        }
    }
}

/**
 * Local Kokoro: one model, eleven voices. A voice is a half-megabyte style vector
 * inside the bundle's `voices.bin`, not a separate download — so the whole
 * catalogue arrives with the model and switching between them is instant.
 */
@Composable
private fun LocalKokoro(
    settings: Prefs,
    controller: AppController,
    models: Map<String, TtsModelState>,
) {
    val model = TtsCatalogue.KOKORO_MODEL
    val state = models[model.id] ?: TtsModelState(model.id)
    Text(
        "Kokoro running on this phone — the same model the server uses, so the voices "
            + "match, and it works with no connection at all. Synthesis is roughly "
            + "real-time on device, so long replies start speaking quickly but finish "
            + "slower than the server would.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
    TtsModelRow(model, state, controller)
    if (!state.installed) return

    var voice by remember { mutableStateOf(settings.kokoroVoice) }
    var open by remember { mutableStateOf(false) }
    Box {
        OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) {
            Text("Voice: $voice ▾")
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            TtsCatalogue.KOKORO_VOICES.forEach { v ->
                DropdownMenuItem(text = { Text(v) }, onClick = {
                    voice = v; settings.kokoroVoice = v; open = false
                    controller.previewTtsVoice(v)
                })
            }
        }
    }
}

/**
 * Local Piper: several small single-voice models. Here the *model* is the voice,
 * so picking a voice means having downloaded that one — each is about a fifth of
 * Kokoro's size and several times faster to synthesize.
 */
@Composable
private fun LocalPiper(
    settings: Prefs,
    controller: AppController,
    models: Map<String, TtsModelState>,
) {
    var chosen by remember { mutableStateOf(settings.piperModel) }
    Text(
        "Piper voices running on this phone. Each voice is its own small model — much "
            + "faster than Kokoro and a fraction of the size, at some cost in how natural "
            + "it sounds. Download the ones you want to compare.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
    TtsCatalogue.PIPER_MODELS.forEach { model ->
        val state = models[model.id] ?: TtsModelState(model.id)
        Row(verticalAlignment = Alignment.CenterVertically) {
            RadioButton(
                // Only show as chosen once it's actually usable — a selected-looking
                // radio on a voice that isn't downloaded would be a lie, since
                // speech would silently come out in the Android voice instead.
                selected = chosen == model.id && state.installed,
                // Selecting a voice you haven't downloaded would just fall through
                // to the Android engine, so gate it on the model being present.
                enabled = state.installed,
                onClick = {
                    chosen = model.id; settings.piperModel = model.id
                    controller.previewTtsVoice("")
                },
            )
            Column(Modifier.weight(1f)) { TtsModelRow(model, state, controller, compact = true) }
        }
    }
}

/**
 * One downloadable model: its size, and whichever of download / progress /
 * delete applies. Errors stay on the row rather than becoming a toast, because
 * the fix (retry, or free some space) is right there.
 */
@Composable
private fun TtsModelRow(
    model: TtsModel,
    state: TtsModelState,
    controller: AppController,
    compact: Boolean = false,
) {
    Column(Modifier.fillMaxWidth().padding(vertical = 4.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text(
                    if (compact) model.label else "${model.label} · ${mb(model.bytes)}",
                    style = MaterialTheme.typography.bodyLarge,
                )
                if (model.note.isNotEmpty()) {
                    Text(
                        if (compact) "${model.note} · ${mb(model.bytes)}" else model.note,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.outline,
                    )
                }
            }
            when {
                state.downloading -> Text(
                    percent(state), style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.outline,
                )
                state.installed -> TextButton(onClick = { controller.removeLocalTtsModel(model.id) }) {
                    Text("Delete")
                }
                else -> Button(onClick = { controller.installLocalTtsModel(model.id) }) {
                    Text("Download")
                }
            }
        }
        if (state.downloading) {
            val total = state.total.takeIf { it > 0 } ?: model.bytes
            LinearProgressIndicator(
                progress = { (state.received.toFloat() / total).coerceIn(0f, 1f) },
                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
            )
        }
        if (state.error.isNotEmpty()) {
            Text(
                "Download failed: ${state.error}",
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.error,
            )
        }
    }
}

private fun mb(bytes: Long) = "${bytes / 1_000_000} MB"

private fun percent(state: TtsModelState): String {
    val total = state.total
    if (total <= 0) return "…"
    return "${(state.received * 100 / total).coerceIn(0, 100)}%"
}

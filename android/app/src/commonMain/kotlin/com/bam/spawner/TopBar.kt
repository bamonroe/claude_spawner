package com.bam.spawner

import androidx.compose.foundation.basicMarquee
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AcUnit
import androidx.compose.material.icons.filled.Bolt
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Menu
import androidx.compose.material.icons.filled.Psychology
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import kotlinx.coroutines.delay

/** The top app bar: menu, session title/subtitle, current context size, audio-output picker, settings. */
@Composable
fun TopBar(
    title: String,
    subtitle: String,
    modelBadge: String,
    contextTokens: Int?,
    onMenu: (() -> Unit)?,
    onSettings: () -> Unit,
    audioOutput: AudioOutput,
    audioOutputs: List<AudioOutput>,
    onSelectOutput: (AudioOutput) -> Unit,
    audioInput: AudioInput,
    audioInputs: List<AudioInput>,
    onSelectInput: (AudioInput) -> Unit,
    onOutputMenuOpened: () -> Unit,
) {
    Surface(tonalElevation = 2.dp) {
        Row(
            Modifier.fillMaxWidth().padding(horizontal = 4.dp, vertical = 2.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            // Wide/desktop layouts pin a persistent sidebar and pass null here, so the
            // ☰ toggle disappears (there's no drawer to open).
            if (onMenu != null) IconButton(onClick = onMenu) { Icon(Icons.Filled.Menu, contentDescription = "Menu") }
            Column(Modifier.weight(1f)) {
                Text(title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text("· $subtitle", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
            }
            // Attached session's backend/model badge, so the current AI + model is
            // visible without letting long provider/model names widen the top bar.
            if (modelBadge.isNotEmpty()) Box(
                Modifier
                    .width(126.dp)
                    .padding(horizontal = 6.dp)
                    .clipToBounds(),
            ) {
                Text(
                    modelBadge,
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.secondary,
                    maxLines = 1,
                    overflow = TextOverflow.Clip,
                    modifier = Modifier.basicMarquee(iterations = Int.MAX_VALUE),
                )
            }
            // Current context size — the last turn's context tokens (input + cache).
            if (contextTokens != null && contextTokens > 0) Row(
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.padding(horizontal = 6.dp),
            ) {
                Icon(
                    Icons.Filled.Psychology, contentDescription = null,
                    tint = MaterialTheme.colorScheme.outline, modifier = Modifier.size(16.dp),
                )
                Spacer(Modifier.width(3.dp))
                Text(
                    fmtTok(contextTokens),
                    style = MaterialTheme.typography.labelMedium,
                    color = MaterialTheme.colorScheme.outline,
                )
            }
            AudioRouteButton(
                audioOutput, audioOutputs, onSelectOutput,
                audioInput, audioInputs, onSelectInput, onOutputMenuOpened,
            )
            IconButton(onClick = onSettings) { Icon(Icons.Filled.Settings, contentDescription = "Settings") }
        }
    }
}

/** Top-bar button showing the current spoken-audio output; tap for a two-section
 *  picker — Output (where the voice plays) and Input (which mic captures) — so the
 *  two are chosen explicitly and independently. Headset entries appear only while a
 *  headset is connected. Picks don't dismiss the menu, so both can be set in one
 *  visit. The Input section is hidden when [inputs] is empty (clients that don't
 *  capture audio, e.g. the browser), leaving a plain output menu. */
@Composable
fun AudioRouteButton(
    output: AudioOutput,
    outputs: List<AudioOutput>,
    onSelectOutput: (AudioOutput) -> Unit,
    input: AudioInput,
    inputs: List<AudioInput>,
    onSelectInput: (AudioInput) -> Unit,
    onOpened: () -> Unit,
) {
    var open by remember { mutableStateOf(false) }
    val showInput = inputs.isNotEmpty()
    Box {
        IconButton(onClick = { onOpened(); open = true }) {
            val desc = if (showInput) "Audio: output ${output.label}, input ${input.label}"
                else "Audio output: ${output.label}"
            Icon(output.icon, contentDescription = desc)
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            if (showInput) RouteSectionHeader("Output")
            outputs.forEach { out ->
                RouteItem(out.icon, out.label, out == output) { onSelectOutput(out) }
            }
            if (showInput) {
                HorizontalDivider(Modifier.padding(vertical = 4.dp))
                RouteSectionHeader("Input")
                inputs.forEach { inp ->
                    RouteItem(inp.icon, inp.label, inp == input) { onSelectInput(inp) }
                }
            }
        }
    }
}

/** A caption labeling one section of the audio-route picker. */
@Composable
private fun RouteSectionHeader(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.labelSmall,
        color = MaterialTheme.colorScheme.outline,
        modifier = Modifier.padding(start = 12.dp, top = 6.dp, bottom = 2.dp),
    )
}

/** One selectable route in the picker: icon + label, with a check when active.
 *  Selecting keeps the menu open so the other section can also be set. */
@Composable
private fun RouteItem(icon: ImageVector, label: String, selected: Boolean, onClick: () -> Unit) {
    DropdownMenuItem(
        text = {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(icon, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(8.dp))
                Text(label)
                if (selected) {
                    Spacer(Modifier.width(6.dp))
                    Icon(Icons.Filled.Check, contentDescription = "selected", modifier = Modifier.size(16.dp))
                }
            }
        },
        onClick = onClick,
    )
}

// CacheWarmBar counts down the ~5-minute window in which the next turn reuses the
// warm prompt cache (a cache_read hit) rather than rebuilding context. Driven off
// the last turn's completion time; ticks once a second. See Appearance settings.
@Composable
fun CacheWarmBar(info: TurnUsageInfo) {
    val windowMs = 5 * 60 * 1000L
    var now by remember { mutableStateOf(nowMonotonicMs()) }
    LaunchedEffect(info) {
        while (true) {
            now = nowMonotonicMs()
            delay(1000)
        }
    }
    val remaining = (windowMs - (now - info.atElapsedMs)).coerceAtLeast(0)
    val warm = remaining > 0
    val label = if (warm) {
        val secs = (remaining % 60000) / 1000
        "cache warm · ${remaining / 60000}:${secs.toString().padStart(2, '0')} left"
    } else {
        "cache cold — next turn rebuilds context"
    }
    // Warm = orange (hot cache), cold = blue (chilled) — temperature cues, not the theme accent.
    val color = if (warm) BuddyOrange else Color(0xFF42A5F5)
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
    ) {
        Icon(
            if (warm) Icons.Filled.Bolt else Icons.Filled.AcUnit, contentDescription = null,
            tint = color, modifier = Modifier.size(14.dp),
        )
        Spacer(Modifier.width(4.dp))
        Text(label, color = color, style = MaterialTheme.typography.labelMedium)
    }
}

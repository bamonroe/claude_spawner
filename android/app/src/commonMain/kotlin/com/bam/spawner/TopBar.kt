package com.bam.spawner

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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
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
            // always visible (blank when detached or on a pre-agent server).
            if (modelBadge.isNotEmpty()) Text(
                modelBadge,
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.secondary,
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
                modifier = Modifier.padding(horizontal = 6.dp),
            )
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
            AudioOutputButton(audioOutput, audioOutputs, onSelectOutput, onOutputMenuOpened)
            IconButton(onClick = onSettings) { Icon(Icons.Filled.Settings, contentDescription = "Settings") }
        }
    }
}

/** Top-bar button showing the current spoken-audio output; tap to pick another
 *  (Bluetooth appears only while a headset is connected). */
@Composable
fun AudioOutputButton(
    current: AudioOutput,
    outputs: List<AudioOutput>,
    onSelect: (AudioOutput) -> Unit,
    onOpened: () -> Unit,
) {
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { onOpened(); open = true }) {
            Icon(current.icon, contentDescription = "Audio output: ${current.label}")
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            outputs.forEach { out ->
                DropdownMenuItem(
                    text = {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Icon(out.icon, contentDescription = null, modifier = Modifier.size(18.dp))
                            Spacer(Modifier.width(8.dp))
                            Text(out.label)
                            if (out == current) {
                                Spacer(Modifier.width(6.dp))
                                Icon(Icons.Filled.Check, contentDescription = "selected", modifier = Modifier.size(16.dp))
                            }
                        }
                    },
                    onClick = { onSelect(out); open = false },
                )
            }
        }
    }
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

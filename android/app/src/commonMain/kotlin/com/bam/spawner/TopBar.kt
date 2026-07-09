package com.bam.spawner

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.bam.spawner.audio.AudioOutput
import kotlinx.coroutines.delay

/** The top app bar: menu, session title/subtitle, current context size, audio-output picker, settings. */
@Composable
fun TopBar(
    title: String,
    subtitle: String,
    contextTokens: Int?,
    onMenu: () -> Unit,
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
            TextButton(onClick = onMenu) { Text("☰", fontSize = 22.sp) }
            Column(Modifier.weight(1f)) {
                Text(title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text("· $subtitle", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
            }
            // Current context size — the last turn's context tokens (input + cache).
            if (contextTokens != null && contextTokens > 0) Text(
                "🧠 ${fmtTok(contextTokens)}",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.outline,
                modifier = Modifier.padding(horizontal = 6.dp),
            )
            AudioOutputButton(audioOutput, audioOutputs, onSelectOutput, onOutputMenuOpened)
            TextButton(onClick = onSettings) { Text("⚙", fontSize = 20.sp) }
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
        TextButton(onClick = { onOpened(); open = true }) { Text(current.icon, fontSize = 18.sp) }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            outputs.forEach { out ->
                DropdownMenuItem(
                    text = { Text("${out.icon}  ${out.label}${if (out == current) "  ✓" else ""}") },
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
        "⚡ cache warm · ${remaining / 60000}:${secs.toString().padStart(2, '0')} left"
    } else {
        "❄ cache cold — next turn rebuilds context"
    }
    Text(
        label,
        color = if (warm) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outline,
        style = MaterialTheme.typography.labelMedium,
        modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
    )
}

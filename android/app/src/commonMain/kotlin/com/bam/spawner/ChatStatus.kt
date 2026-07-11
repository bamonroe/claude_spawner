package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.Compress
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.LockOpen
import androidx.compose.material.icons.filled.MenuBook
import androidx.compose.material.icons.filled.MoreHoriz
import androidx.compose.material.icons.filled.Psychology
import androidx.compose.material.icons.filled.Public
import androidx.compose.material.icons.filled.Search
import androidx.compose.material.icons.filled.SmartToy
import androidx.compose.material.icons.filled.Stop
import androidx.compose.material.icons.filled.Terminal
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.FilterChip
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.bam.spawner.net.AskQuestion

/** Shown when no session is attached: a safe "command mode" — utterances are
 * commands (no "hey buddy" needed) and nothing reaches a Claude session. */
@Composable
fun DetachedBanner() {
    Surface(color = Color(0xFF2E7D32), contentColor = Color.White, modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(Icons.Filled.LockOpen, contentDescription = null, modifier = Modifier.size(16.dp))
            Spacer(Modifier.width(4.dp))
            Text(
                "Detached — command mode. Speak commands directly (no \"hey buddy\" needed); " +
                    "nothing goes to a Claude session.",
                style = MaterialTheme.typography.bodySmall,
            )
        }
    }
}

/** Shown while TTS is speaking: a full-width tap target that stops the readout. */
@Composable
fun SpeakingBar(onStop: () -> Unit) {
    Surface(
        color = MaterialTheme.colorScheme.secondaryContainer,
        shape = RoundedCornerShape(14.dp),
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp).clickable { onStop() },
    ) {
        Row(
            Modifier.padding(horizontal = 12.dp, vertical = 10.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                Icons.AutoMirrored.Filled.VolumeUp, contentDescription = null,
                tint = MaterialTheme.colorScheme.onSecondaryContainer, modifier = Modifier.size(18.dp),
            )
            Spacer(Modifier.width(4.dp))
            Text(
                "Speaking — tap to stop",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSecondaryContainer,
            )
        }
    }
}

/** The server tags each live-activity line with a leading emoji as a compact "kind"
 *  marker (see gateway's `toolActivity`). We translate that marker to a Material vector
 *  icon and strip it from the label, so the bubble reads as an icon + text, not an emoji. */
private fun activityIcon(text: String): Pair<ImageVector, String> {
    val icon = when {
        text.startsWith("🤔") -> Icons.Filled.Psychology     // thinking / working
        text.startsWith("✏️") -> Icons.Filled.Edit            // editing a file
        text.startsWith("🗜️") -> Icons.Filled.Compress        // compressing context
        text.startsWith("⚙️") -> Icons.Filled.Terminal        // running a command
        text.startsWith("📖") -> Icons.Filled.MenuBook        // reading a file
        text.startsWith("🔍") -> Icons.Filled.Search          // searching the code
        text.startsWith("🌐") -> Icons.Filled.Public          // searching the web
        text.startsWith("🤖") -> Icons.Filled.SmartToy        // running a subtask
        else -> Icons.Filled.MoreHoriz                        // "· ToolName…" fallback
    }
    // Drop the leading marker token (emoji or "·") and the space after it.
    val label = text.replaceFirst(Regex("^\\S+\\s+"), "")
    return icon to label
}

/** Live "Claude is thinking / editing foo.go" indicator, like a typing bubble. */
@Composable
fun ActivityIndicator(text: String, onAbort: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Surface(
            color = MaterialTheme.colorScheme.surfaceVariant,
            shape = RoundedCornerShape(14.dp),
        ) {
            Row(
                Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                val (icon, label) = activityIcon(text)
                Icon(
                    icon, contentDescription = null, modifier = Modifier.size(16.dp),
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Spacer(Modifier.width(6.dp))
                Text(
                    label,
                    style = MaterialTheme.typography.bodyMedium,
                    fontStyle = FontStyle.Italic,
                )
            }
        }
        TextButton(onClick = onAbort) {
            Icon(Icons.Filled.Stop, contentDescription = null, modifier = Modifier.size(16.dp))
            Spacer(Modifier.width(4.dp))
            Text("stop", fontSize = 13.sp)
        }
    }
}

/** Interactive-mode clarification questions: chips for multiple-choice, text
 *  fields otherwise. Also read aloud, so you can just answer by voice (which
 *  dismisses this). */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun AskDialog(questions: List<AskQuestion>, onSubmit: (String) -> Unit, onDismiss: () -> Unit) {
    val answers = remember(questions) { mutableStateListOf<String>().apply { repeat(questions.size) { add("") } } }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Claude needs input") },
        text = {
            Column(
                Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                questions.forEachIndexed { i, q ->
                    Text(q.q, style = MaterialTheme.typography.bodyLarge)
                    if (q.options.isEmpty()) {
                        OutlinedTextField(answers[i], { answers[i] = it }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    } else {
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                            q.options.forEach { opt ->
                                FilterChip(selected = answers[i] == opt, onClick = { answers[i] = opt }, label = { Text(opt) })
                            }
                        }
                    }
                }
                Text("…or just answer out loud.", style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.outline)
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    val text = questions.mapIndexed { i, q ->
                        "Q: ${q.q}\nA: ${answers[i].ifBlank { "(no preference)" }}"
                    }.joinToString("\n\n")
                    onSubmit(text)
                },
                enabled = answers.any { it.isNotBlank() },
            ) { Text("Send") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Dismiss") } },
    )
}

/** The live hands-free draft — captured-but-uncommitted text, shown greyed above
 *  the input bar so you can see what's buffered before you say the end token. */
@Composable
fun DraftLine(text: String) {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f),
        shape = RoundedCornerShape(10.dp),
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 2.dp),
    ) {
        Row(
            Modifier.padding(horizontal = 10.dp, vertical = 6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(
                Icons.Filled.Edit, contentDescription = null,
                tint = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.8f),
                modifier = Modifier.size(16.dp),
            )
            Spacer(Modifier.width(4.dp))
            Text(
                text,
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.8f),
                fontStyle = FontStyle.Italic,
            )
        }
    }
}

/** Compact hands-free status pill: Listening / Capturing / Transcribing / Thinking / Speaking. */
@Composable
fun VoiceStatePill(state: VoiceState) {
    val (label, dot) = when (state) {
        VoiceState.OFF -> return
        VoiceState.LISTENING -> "listening for \"hey buddy\"" to Color(0xFF4CAF50)
        VoiceState.CAPTURING -> "listening to you…" to Color(0xFF2196F3)
        VoiceState.TRANSCRIBING -> "transcribing…" to Color(0xFF00ACC1)
        VoiceState.THINKING -> "thinking…" to Color(0xFFFFB300)
        VoiceState.SPEAKING -> "speaking… (talk to interrupt)" to Color(0xFF9C27B0)
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(8.dp).background(dot, CircleShape))
        Text(
            "  $label", style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

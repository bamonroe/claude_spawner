package com.bam.spawner

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.StarBorder
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp

// The `Command` type and the alphabetical `COMMANDS` list are GENERATED at build
// time from docs/commands.json (see the generateCommands Gradle task), whose
// source of truth is the server's command registry. Don't hand-maintain a list
// here — add commands in the server registry so the app can never drift.

/**
 * Commands reference + per-command alias editor (fixes whisper mis-hears), plus
 * the two spoken tokens that bracket a command: the **wake token** that opens one
 * and the **end token** that commits a hands-free message. Both ride the hello
 * handshake, so applying either reconnects via [onSttChanged]. [endTokenTest] is
 * the platform calibration "Test" slot (Android only; web passes the empty slot).
 */
@Composable
fun CommandsSettings(
    settings: Prefs,
    onAliasesChanged: () -> Unit,
    onSttChanged: () -> Unit,
    onBack: () -> Unit,
    endTokenTest: @Composable (String) -> Unit = {},
) {
    var aliasMap by remember { mutableStateOf(settings.aliasMap()) }
    var trayNames by remember { mutableStateOf(settings.trayCommandNames().toSet()) }
    var gate by remember { mutableStateOf(settings.dictationGate) }
    var useDetector by remember { mutableStateOf(settings.wakeService == "detector") }
    var silence by remember { mutableStateOf(if (settings.silenceCommitSeconds <= 0f) "" else settings.silenceCommitSeconds.toString()) }
    SettingsScaffold("Commands", onBack) {
        Text("Say your wake word → a command → your end token.", style = MaterialTheme.typography.bodyMedium)
        Text(
            "The wake, end and speech-gate phrases themselves live in Settings → Spoken tokens now.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

        HorizontalDivider()
        Text("Wake/end-token detection", style = MaterialTheme.typography.titleMedium)
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Use dedicated wake-word detector", style = MaterialTheme.typography.bodyLarge)
                Text("Off (default) scores the live wake/end tokens by string-matching the fast "
                    + "Whisper transcript — always available. On uses the purpose-trained LiveKit "
                    + "detector sidecar, which needs the server's wake-word service configured.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = useDetector, onCheckedChange = {
                useDetector = it
                settings.wakeService = if (it) "detector" else "whisper"
                onSttChanged()
            })
        }

        HorizontalDivider()
        Text("Dictation gate", style = MaterialTheme.typography.titleMedium)
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Require a speak token", style = MaterialTheme.typography.bodyLarge)
                Text("Only send speech that follows the speak token (up to the end token). "
                    + "Everything else — background chatter, radio, other people — is discarded.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = gate, onCheckedChange = { gate = it; settings.dictationGate = it; onSttChanged() })
        }
        Text(
            "Configure the speech-gate phrase(s) in Settings → Spoken tokens — say one to start "
                + "dictating, then your end token to send (e.g. \"take a note … beep\"). "
                + "Commands (\"hey buddy …\") always work, gate or no gate.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

        HorizontalDivider()
        Text("Silence auto-commit", style = MaterialTheme.typography.titleMedium)
        OutlinedTextField(
            silence,
            { silence = it; settings.silenceCommitSeconds = it.toFloatOrNull() ?: 0f },
            label = { Text("Silence auto-commit (seconds, 0 = off)") },
            singleLine = true,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Decimal),
            modifier = Modifier.fillMaxWidth(),
        )
        Text("Commits after this much quiet. Blank/0 = only the end token commits.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

        HorizontalDivider()
        Text(
            "Tap a command to expand it — add aliases for words whisper mis-hears, or "
                + "add it to the swipe-up tray. Tap an alias bubble to remove it.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        COMMANDS.forEach { cmd ->
            CommandAliasGroup(
                cmd = cmd,
                aliases = aliasMap.filterValues { it == cmd.name }.keys.sorted(),
                inTray = cmd.name in trayNames,
                onToggleTray = { on ->
                    settings.setTrayCommand(cmd.name, on); trayNames = settings.trayCommandNames().toSet()
                },
                onAdd = { misheard ->
                    settings.addAlias(misheard, cmd.name); aliasMap = settings.aliasMap(); onAliasesChanged()
                },
                onRemove = { misheard ->
                    settings.removeAlias(misheard); aliasMap = settings.aliasMap(); onAliasesChanged()
                },
            )
        }
    }
}

/**
 * One command as a collapsible card. Collapsed it shows just the name, its
 * description, and a tray marker; tapping the header expands it to reveal the
 * spoken aliases, the alias editor (add/remove whisper mis-hears), and the
 * "add to tray" toggle. Only argument-free commands can join the swipe-up tray —
 * a tray button can't supply a `<name>`/`<dir>` — so those show a note instead.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun CommandAliasGroup(
    cmd: Command,
    aliases: List<String>,
    inTray: Boolean,
    onToggleTray: (Boolean) -> Unit,
    onAdd: (String) -> Unit,
    onRemove: (String) -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    var adding by remember { mutableStateOf(false) }
    val trayable = cmd.aliases.none { it.contains("<") }
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.35f),
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            // Header — tap anywhere on it to expand/collapse.
            Row(
                Modifier.fillMaxWidth().clickable { expanded = !expanded },
                verticalAlignment = Alignment.CenterVertically,
            ) {
                Column(Modifier.weight(1f)) {
                    Text(cmd.name, style = MaterialTheme.typography.titleMedium)
                    Text(cmd.description, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                }
                if (inTray) {
                    Icon(
                        Icons.Filled.Star, contentDescription = "In tray",
                        tint = MaterialTheme.colorScheme.primary,
                        modifier = Modifier.size(18.dp),
                    )
                }
                Icon(
                    if (expanded) Icons.Filled.KeyboardArrowUp else Icons.Filled.KeyboardArrowDown,
                    contentDescription = if (expanded) "Collapse" else "Expand",
                )
            }
            if (expanded) {
                Text(
                    "say: " + cmd.aliases.joinToString(" · "),
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                )
                // Add-to-tray toggle (argument-free commands only).
                if (trayable) {
                    OutlinedButton(onClick = { onToggleTray(!inTray) }) {
                        Icon(
                            if (inTray) Icons.Filled.Star else Icons.Filled.StarBorder,
                            contentDescription = null, modifier = Modifier.size(18.dp),
                        )
                        Spacer(Modifier.width(6.dp))
                        Text(if (inTray) "Remove from tray" else "Add to tray")
                    }
                } else {
                    Text(
                        "Takes a spoken argument, so it can't be a one-tap tray button.",
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                    )
                }
                // Alias editor.
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text("Aliases", Modifier.weight(1f), style = MaterialTheme.typography.labelLarge)
                    OutlinedButton(onClick = { adding = true }) { Icon(Icons.Filled.Add, contentDescription = "Add alias") }
                }
                if (aliases.isNotEmpty()) {
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        aliases.forEach { AliasChip(it) { onRemove(it) } }
                    }
                }
            }
        }
    }
    if (adding) {
        AddAliasForCommandDialog(
            command = cmd.name,
            onAdd = { onAdd(it); adding = false },
            onDismiss = { adding = false },
        )
    }
}

/** A removable alias bubble — tap to confirm removal. */
@Composable
private fun AliasChip(text: String, onRemove: () -> Unit) {
    var confirm by remember { mutableStateOf(false) }
    Surface(
        onClick = { confirm = true },
        color = MaterialTheme.colorScheme.secondaryContainer,
        contentColor = MaterialTheme.colorScheme.onSecondaryContainer,
        shape = RoundedCornerShape(16.dp),
    ) {
        Text(text, Modifier.padding(horizontal = 12.dp, vertical = 6.dp), style = MaterialTheme.typography.bodyMedium)
    }
    if (confirm) {
        AlertDialog(
            onDismissRequest = { confirm = false },
            title = { Text("Remove alias?") },
            text = { Text("Remove \"$text\"?") },
            confirmButton = { TextButton(onClick = { onRemove(); confirm = false }) { Text("Remove") } },
            dismissButton = { TextButton(onClick = { confirm = false }) { Text("Cancel") } },
        )
    }
}

@Composable
private fun AddAliasForCommandDialog(command: String, onAdd: (String) -> Unit, onDismiss: () -> Unit) {
    var misheard by remember { mutableStateOf("") }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Alias for \"$command\"") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("What does whisper hear instead of \"$command\"?", style = MaterialTheme.typography.bodySmall)
                OutlinedTextField(misheard, { misheard = it }, label = { Text("Misheard word / phrase") }, singleLine = true)
            }
        },
        confirmButton = { TextButton(onClick = { if (misheard.isNotBlank()) onAdd(misheard) }) { Text("Add") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

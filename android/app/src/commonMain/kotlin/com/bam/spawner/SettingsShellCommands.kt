package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.ShellCommandInfo
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice the shared Shell-commands editor (Settings → Shell commands) needs. The
 * app-managed catalogue is the **closed set** a shell token may run on the target host,
 * which is what makes spoken shell safe — an arbitrary spoken command line is never
 * runnable. Both clients edit the same server-persisted list; the server broadcasts an
 * updated `shell_commands` after every change.
 */
interface ShellCommandsController {
    val connected: StateFlow<Boolean>
    val shellCommands: StateFlow<List<ShellCommandInfo>>
    fun putShellCommand(c: ShellCommandInfo)
    fun deleteShellCommand(name: String)
}

/**
 * Settings → Shell commands. Each record binds a spoken alias to a fixed command-line
 * template run over SSH on the shell target (or on a pinned host), optionally in a set
 * working directory. Spoken arguments fill `$1`…`$9` / `$*` in the template and are
 * shell-quoted, so an argument can never inject an operator or a second command.
 */
@Composable
fun ShellCommandsSettings(controller: ShellCommandsController, onBack: () -> Unit) {
    val commands by controller.shellCommands.collectAsState()
    val connected by controller.connected.collectAsState()

    var alias by rememberSaveable { mutableStateOf("") }
    var command by rememberSaveable { mutableStateOf("") }
    var dir by rememberSaveable { mutableStateOf("") }
    var host by rememberSaveable { mutableStateOf("") }
    var editing by rememberSaveable { mutableStateOf("") } // alias being edited, "" = new
    var showForm by rememberSaveable { mutableStateOf(false) }
    val clear = { alias = ""; command = ""; dir = ""; host = ""; editing = "" }

    SettingsScaffold("Shell commands", onBack) {
        Text(
            "Say a shell token followed by one of these aliases to run it on the target host. Only the "
                + "commands listed here can ever run — the catalogue is the safety boundary, so spoken shell "
                + "is never arbitrary. The app owns this list; the server shares it across devices.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage shell commands.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }

        HorizontalDivider()
        if (commands.isEmpty()) {
            Text("None yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (c in commands) {
            Surface(
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                shape = RoundedCornerShape(12.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Row(Modifier.padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) {
                        Text(c.name, style = MaterialTheme.typography.titleMedium)
                        Text(c.command, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                        val where = listOfNotNull(
                            c.host.takeIf { it.isNotBlank() }?.let { "on $it" },
                            c.dir.takeIf { it.isNotBlank() }?.let { "in $it" },
                        ).joinToString(" ")
                        if (where.isNotBlank()) {
                            Text(where, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                        }
                    }
                    TextButton(onClick = {
                        alias = c.name; command = c.command; dir = c.dir; host = c.host; editing = c.name; showForm = true
                    }) { Text("Edit") }
                    TextButton(onClick = {
                        controller.deleteShellCommand(c.name)
                        if (editing == c.name) { clear(); showForm = false }
                    }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                }
            }
        }

        HorizontalDivider()
        if (!showForm) {
            Button(enabled = connected, onClick = { clear(); showForm = true }) { Text("Add command") }
        } else {
            Text(if (editing.isBlank()) "Add command" else "Editing “$editing”", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(alias, { alias = it }, label = { Text("Spoken alias (e.g. disk space)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(command, { command = it }, label = { Text("Command template") }, modifier = Modifier.fillMaxWidth())
            Text(
                "\$1…\$9 take the Nth spoken argument, \$* the rest space-joined, \$\$ a literal dollar. "
                    + "Arguments are shell-quoted, so they can't add an operator or a second command.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
            )
            OutlinedTextField(dir, { dir = it }, label = { Text("Directory (blank = login default)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(host, { host = it }, label = { Text("Host (blank = the shell target)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && alias.isNotBlank() && command.isNotBlank(),
                    onClick = {
                        // Renaming the alias means a new record: the name IS the key, so drop the old one.
                        val newName = alias.trim()
                        if (editing.isNotBlank() && editing != newName) controller.deleteShellCommand(editing)
                        controller.putShellCommand(
                            ShellCommandInfo(name = newName, command = command.trim(), dir = dir.trim(), host = host.trim()),
                        )
                        clear(); showForm = false
                    },
                ) { Text(if (editing.isBlank()) "Add" else "Save") }
                OutlinedButton(onClick = { clear(); showForm = false }) { Text("Cancel") }
            }
        }
    }
}

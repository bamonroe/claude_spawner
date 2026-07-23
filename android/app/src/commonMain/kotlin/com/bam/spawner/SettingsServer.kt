package com.bam.spawner

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType

/**
 * Server connection settings: URL/token + Save & Connect, context-compression triggers, the
 * session-behavior toggles (brief replies, ask before guessing), and the restart button. TLS is
 * terminated at the reverse proxy (Caddy), so the app just speaks `wss://` and authenticates with
 * the token — no client cert.
 */
@Composable
fun ServerSettings(
    settings: Prefs,
    controller: AppController,
    onSaveConnect: (String, String) -> Unit,
    onSttChanged: () -> Unit,
    onBack: () -> Unit,
    // Platform slot for the "Trust CA" section — Android fills it (import a private
    // CA so a `tls internal` wss server validates); the browser leaves it empty.
    caSection: @Composable ColumnScope.() -> Unit = {},
) {
    var url by rememberSaveable { mutableStateOf(settings.url) }
    var token by rememberSaveable { mutableStateOf(settings.token) }
    val connected by controller.connected.collectAsState()
    // The pending restart mode awaiting confirmation ("build" | "bounce" | "rebuild"), or null.
    var restartMode by remember { mutableStateOf<String?>(null) }
    SettingsScaffold("Server", onBack) {
        OutlinedTextField(url, { url = it }, label = { Text("Server URL") }, placeholder = { Text("cs.bam") }, supportingText = { Text("Host is enough — wss:// and /ws are added. Add :port for a plain-ws direct connection.") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(token, { token = it }, label = { Text("Token") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        Button(onClick = {
            settings.url = url; settings.token = token
            onSaveConnect(url, token)
        }) {
            Text("Save & Connect")
        }
        Text("Client ID: ${settings.clientId}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

        caSection()

        HorizontalDivider()
        Text("Context compression", style = MaterialTheme.typography.titleMedium)
        Text("Server-global. Both triggers share the token limit below.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        var warmCompress by remember { mutableStateOf(settings.warmCompress) }
        var autoCompress by remember { mutableStateOf(settings.autoCompress) }
        var compressLimit by remember { mutableStateOf(settings.autoCompressThreshold.toString()) }
        val pushCompress = {
            controller.setAutoCompress(warmCompress, autoCompress, compressLimit.toIntOrNull() ?: 0)
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Warm compress", style = MaterialTheme.typography.titleMedium)
                Text("Past the limit, compress just before the warm cache expires — reuses the warm cache instead of a cold rebuild.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = warmCompress, onCheckedChange = { warmCompress = it; settings.warmCompress = it; pushCompress() })
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Auto compress", style = MaterialTheme.typography.titleMedium)
                Text("Compress as soon as a session crosses the limit, without waiting for the warm window. Wins over warm compress if both are on.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = autoCompress, onCheckedChange = { autoCompress = it; settings.autoCompress = it; pushCompress() })
        }
        if (warmCompress || autoCompress) {
            OutlinedTextField(
                value = compressLimit,
                onValueChange = { v ->
                    compressLimit = v.filter { it.isDigit() }.take(4)
                    settings.autoCompressThreshold = compressLimit.toIntOrNull() ?: 0
                    pushCompress()
                },
                label = { Text("Compress limit (thousands)") },
                suffix = { Text("k tokens") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                modifier = Modifier.fillMaxWidth(),
            )
        }

        HorizontalDivider()
        Text("Session behavior", style = MaterialTheme.typography.titleMedium)
        var brief by remember { mutableStateOf(settings.brief) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Brief replies", style = MaterialTheme.typography.titleMedium)
                Text("Ask Claude to keep answers short, for text-to-speech.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = brief, onCheckedChange = { brief = it; settings.brief = it; onSttChanged() })
        }
        var interactive by remember { mutableStateOf(settings.interactive) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Ask before guessing", style = MaterialTheme.typography.titleMedium)
                Text("Let Claude ask clarifying questions mid-task instead of guessing.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = interactive, onCheckedChange = { interactive = it; settings.interactive = it; onSttChanged() })
        }

        HorizontalDivider()
        Text("Restart server", style = MaterialTheme.typography.titleMedium)
        Text(
            "The server runs in a container. Rebuild compiles current code into a new image "
                + "without touching the running container, so your session keeps going. Restart "
                + "container bounces onto the newest image — running turns are interrupted and the "
                + "app reconnects on its own. Rebuild & restart does both.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        Button(
            onClick = { restartMode = "build" },
            enabled = connected,
        ) { Text("Rebuild Server") }
        Button(
            onClick = { restartMode = "bounce" },
            enabled = connected,
            colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error),
        ) { Text("Restart Container") }
        Button(
            onClick = { restartMode = "rebuild" },
            enabled = connected,
            colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error),
        ) { Text("Rebuild & Restart") }
        if (!connected) {
            Text("Connect first.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
    }
    restartMode?.let { mode ->
        val (title, body) = when (mode) {
            "build" -> "Rebuild the server?" to
                ("The server rebuilds its image from current code. The running container is left in "
                    + "place, so your session keeps going and no turn is interrupted. Tap Restart "
                    + "Container afterward to switch onto the new image.")
            "bounce" -> "Restart the container?" to
                ("The server recreates its container from the newest image (no rebuild). Any running "
                    + "turn is interrupted; the app reconnects automatically.")
            else -> "Rebuild and restart?" to
                ("The server rebuilds from current code, then recreates its container. Any running "
                    + "turn is interrupted; the app reconnects automatically.")
        }
        AlertDialog(
            onDismissRequest = { restartMode = null },
            title = { Text(title) },
            text = { Text(body) },
            confirmButton = { TextButton(onClick = { controller.restartServer(mode); restartMode = null }) { Text("Confirm") } },
            dismissButton = { TextButton(onClick = { restartMode = null }) { Text("Cancel") } },
        )
    }
}

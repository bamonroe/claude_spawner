package com.bam.spawner

import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.width
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp

/**
 * Android-only "Trust CA" section for [ServerSettings]. Lets the user import a private
 * CA (e.g. Caddy's local root, downloaded from the caddyedit "Download CA" button) so
 * the app validates a `tls internal` `wss://` server. A CA `adb push`ed to the app's
 * external files dir is auto-imported on connect; this UI is the manual path + status.
 */
@Composable
fun ServerCaSection(settings: SettingsStore, onChanged: () -> Unit) {
    val context = LocalContext.current
    var name by remember { mutableStateOf(settings.caCertName) }
    var error by remember { mutableStateOf<String?>(null) }

    val picker = rememberLauncherForActivityResult(ActivityResultContracts.GetContent()) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        try {
            val bytes = context.contentResolver.openInputStream(uri)?.use { it.readBytes() }
                ?: throw IllegalStateException("could not read the file")
            name = settings.importCaCert(bytes)
            error = null
            onChanged() // reconnect so the new trust anchor takes effect
        } catch (e: Exception) {
            error = e.message ?: "not a valid certificate"
        }
    }

    HorizontalDivider()
    Text("Trusted CA", style = MaterialTheme.typography.titleMedium)
    Text(
        if (name.isBlank()) "None — public certs only. Import a CA to reach a tls-internal server over wss."
        else "Trusting: $name",
        style = MaterialTheme.typography.bodySmall,
        color = MaterialTheme.colorScheme.outline,
    )
    error?.let {
        Text("Import failed: $it", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
    }
    Row {
        OutlinedButton(onClick = { picker.launch("*/*") }) {
            Text(if (name.isBlank()) "Import certificate" else "Replace")
        }
        if (name.isNotBlank()) {
            Spacer(Modifier.width(8.dp))
            OutlinedButton(onClick = { settings.clearCaCert(); name = ""; error = null; onChanged() }) {
                Text("Clear")
            }
        }
    }
}

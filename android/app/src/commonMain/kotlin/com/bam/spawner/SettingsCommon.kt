package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp

/** Common wrapper: back arrow + title over a scrollable column. */
@Composable
fun SettingsScaffold(title: String, onBack: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
    Column(
        Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)
            .systemBarsPadding().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = onBack) {
                Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
            }
            Text(title, style = MaterialTheme.typography.titleLarge)
        }
        content()
    }
}

/** The settings landing screen: a list of rows that open each sub-screen. */
@Composable
fun SettingsHub(onOpen: (String) -> Unit, onBack: () -> Unit) {
    SettingsScaffold("Settings", onBack) {
        SettingsRow("Server", "URL, token, connection") { onOpen("set_server") }
        SettingsRow("Appearance", "Theme") { onOpen("set_appearance") }
        SettingsRow("Commands", "Reference & aliases") { onOpen("set_commands") }
        SettingsRow("Spoken tokens", "Wake / end / speech-gate phrases & models") { onOpen("set_spoken_tokens") }
        SettingsRow("Audio", "Mic meter, thresholds, transcription") { onOpen("set_audio") }
        SettingsRow("Hosts", "SSH targets sessions can run on") { onOpen("set_hosts") }
        SettingsRow("Identities", "SSH keypairs hosts authenticate with") { onOpen("set_identities") }
        SettingsRow("Profiles", "How & where sessions run (sandbox, mounts, env)") { onOpen("set_profiles") }
        SettingsRow("Providers", "AI backends: default model & voice model list") { onOpen("set_providers") }
        SettingsRow("Debug", "Hit-zone overlays & gesture logging") { onOpen("set_debug") }
        SettingsRow("About", "Version & build") { onOpen("set_about") }
    }
}

/** One tappable card in [SettingsHub]. */
@Composable
fun SettingsRow(title: String, subtitle: String, onClick: () -> Unit) {
    Surface(
        onClick = onClick,
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(14.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
    }
}

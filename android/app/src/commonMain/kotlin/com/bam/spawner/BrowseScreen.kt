package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

/**
 * The "new session" browser: pick a target (host vs sandbox), a host, an AI backend
 * and model, then browse that host's filesystem and spawn a session in the chosen
 * directory (or a fresh folder). Fully parameterized off [AppController], so the same
 * screen serves both the Android app and the web client.
 */
@Composable
@OptIn(ExperimentalLayoutApi::class)
fun BrowseScreen(controller: AppController, onStarted: () -> Unit, onBack: () -> Unit) {
    val listing by controller.listing.collectAsState()
    val hosts by controller.hosts.collectAsState()
    var newFolder by remember { mutableStateOf<String?>(null) } // non-null = the New-folder dialog is open
    var sandbox by remember { mutableStateOf(false) } // execution target: host (default) vs sandbox
    var selectedHost by rememberSaveable { mutableStateOf(LOCAL_HOST) } // an explicit host name (LOCAL_HOST = loopback)
    // Keep the pick valid as the registry loads: if the current host isn't in the list
    // (e.g. localhost was deleted), fall back to the first configured host.
    LaunchedEffect(hosts) {
        if (hosts.isNotEmpty() && hosts.none { it.name == selectedHost }) selectedHost = hosts.first().name
    }
    // AI backend + model for the new session (from the `agents` registry). "" = the
    // server default; the model snaps to the chosen backend's default when the
    // backend changes or the registry loads.
    val agents by controller.agents.collectAsState()
    var selectedAgent by rememberSaveable { mutableStateOf("") }
    LaunchedEffect(agents) {
        if (agents.isNotEmpty() && agents.none { it.id == selectedAgent }) selectedAgent = agents.first().id
    }
    val agentInfo = agents.firstOrNull { it.id == selectedAgent }
    var selectedModel by rememberSaveable { mutableStateOf("") }
    LaunchedEffect(selectedAgent, agents) {
        agentInfo?.let { if (it.models.none { m -> m == selectedModel }) selectedModel = it.defaultModel }
    }
    val target = if (sandbox) "sandbox" else "host"
    // A host only applies to the host target (a sandbox runs locally); drop any
    // selection when switching to sandbox so we never send a stale host.
    val spawnHost = if (sandbox) "" else selectedHost
    // Which host's filesystem the picker shows: the chosen host, or localhost for a
    // sandbox (it runs in a local container over the host's files). Browsing is scoped
    // to this host and restarts at its root ("/") whenever it changes, so you always
    // browse the machine the session will run on — not the server's own filesystem.
    val browseHost = if (sandbox) LOCAL_HOST else selectedHost
    LaunchedEffect(Unit) { controller.requestHosts() }
    LaunchedEffect(browseHost) { controller.browse("", browseHost) }

    if (newFolder != null) {
        val parent = listing?.path ?: ""
        AlertDialog(
            onDismissRequest = { newFolder = null },
            title = { Text("New project") },
            text = {
                Column {
                    Text("Create a folder in ${parent.ifEmpty { "…" }} and start a session in it.", style = MaterialTheme.typography.bodySmall)
                    OutlinedTextField(
                        value = newFolder ?: "",
                        onValueChange = { newFolder = it },
                        singleLine = true,
                        label = { Text("folder name") },
                        modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
                    )
                }
            },
            confirmButton = {
                TextButton(
                    enabled = !newFolder.isNullOrBlank(),
                    onClick = { controller.spawnNewFolder(parent, newFolder!!, target, spawnHost, selectedAgent, selectedModel); newFolder = null; onStarted() },
                ) { Text("Create & start") }
            },
            dismissButton = { TextButton(onClick = { newFolder = null }) { Text("Cancel") } },
        )
    }

    Column(
        Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)
            .systemBarsPadding().padding(12.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            TextButton(onClick = onBack) { Text("←", fontSize = 22.sp) }
            Text("New session", style = MaterialTheme.typography.titleLarge)
        }
        // Target + host first: they decide which machine we browse, so they sit above
        // the file list. Changing either re-lists from that host's root.
        Row(
            Modifier.fillMaxWidth().padding(top = 4.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Text(
                if (sandbox) "Run in sandbox" else "Run on host",
                Modifier.weight(1f),
                style = MaterialTheme.typography.bodyMedium,
            )
            Switch(checked = sandbox, onCheckedChange = { sandbox = it })
        }
        // Host picker (host target only): one chip per configured host. localhost is an
        // ordinary, seeded, deletable entry, so it shows up here like any other. Hidden
        // for sandbox and when the registry is empty.
        if (!sandbox && hosts.isNotEmpty()) {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), modifier = Modifier.padding(top = 4.dp)) {
                hosts.forEach { h ->
                    FilterChip(selected = selectedHost == h.name, onClick = { selectedHost = h.name }, label = { Text(h.name) })
                }
            }
        }
        // AI backend picker: one chip per backend, shown only when more than one is
        // available (a single backend needs no choice). Codex/Claude etc.
        if (agents.size > 1) {
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), modifier = Modifier.padding(top = 4.dp)) {
                agents.forEach { a ->
                    FilterChip(selected = selectedAgent == a.id, onClick = { selectedAgent = a.id }, label = { Text(a.name) })
                }
            }
        }
        // Model picker: the chosen backend's model aliases (opus/sonnet/fable, or
        // Codex's presets). Hidden when the backend advertises no models.
        agentInfo?.takeIf { it.models.isNotEmpty() }?.let { a ->
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), modifier = Modifier.padding(top = 4.dp)) {
                a.models.forEach { m ->
                    FilterChip(selected = selectedModel == m, onClick = { selectedModel = m }, label = { Text(m) })
                }
            }
        }
        Text(
            listing?.path ?: "",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.outline,
            maxLines = 1, overflow = TextOverflow.Ellipsis,
            modifier = Modifier.padding(vertical = 4.dp),
        )
        HorizontalDivider()

        LazyColumn(Modifier.weight(1f)) {
            if (!(listing?.parent).isNullOrEmpty()) {
                item {
                    Row(
                        Modifier.fillMaxWidth().clickable { controller.browse(listing?.parent ?: "", browseHost) }.padding(vertical = 12.dp),
                    ) { Text("⬆  ..") }
                }
            }
            items(listing?.entries ?: emptyList()) { e ->
                Row(
                    Modifier.fillMaxWidth().clickable { controller.browse(e.path, browseHost) }.padding(vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically,
                ) {
                    Text(if (e.repo) "📦" else "📁")
                    Text(e.name, Modifier.weight(1f).padding(start = 10.dp))
                    if (e.repo) {
                        Text("git", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.primary)
                    }
                }
            }
        }

        HorizontalDivider()
        val canStart = listing?.path?.isNotEmpty() == true
        Button(
            onClick = { listing?.path?.let { controller.spawnAt(it, target, spawnHost, selectedAgent, selectedModel) }; onStarted() },
            enabled = canStart,
            modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
        ) { Text("Start session here") }
        OutlinedButton(
            onClick = { newFolder = "" },
            enabled = canStart, // create a new folder inside the current directory
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
        ) { Text("New project folder here…") }
    }
}

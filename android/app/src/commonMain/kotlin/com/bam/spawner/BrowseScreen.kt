package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
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
 * The "new session" browser: pick a target (host vs sandbox), a host, an execution
 * profile, an AI backend and model, then browse that host's filesystem and spawn a
 * session in the chosen directory (or a fresh folder). Fully parameterized off
 * [AppController], so the same screen serves both the Android app and the web client.
 */
@Composable
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
    // Execution profile for the new session. The server advertises the built-in
    // default first; keep the selection valid as the catalogue arrives. A selected
    // profile's advisory target updates the target switch, but the user can still
    // flip the switch afterwards.
    val profiles by controller.profiles.collectAsState()
    var selectedProfile by rememberSaveable { mutableStateOf("") }
    LaunchedEffect(profiles) {
        if (profiles.isNotEmpty() && profiles.none { it.name == selectedProfile }) selectedProfile = profiles.first().name
    }
    fun selectProfile(name: String) {
        selectedProfile = name
        profiles.firstOrNull { it.name == name }?.target?.let {
            if (it == "sandbox") sandbox = true
            if (it == "host") sandbox = false
        }
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
                    onClick = { controller.spawnNewFolder(parent, newFolder!!, target, spawnHost, selectedAgent, selectedModel, selectedProfile); newFolder = null; onStarted() },
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
        // Host/profile and provider/model choices are dropdowns instead of chip
        // clouds, keeping the filesystem tree as the main use of this screen.
        val showHostPicker = !sandbox && hosts.isNotEmpty()
        val showProfilePicker = profiles.size > 1
        if (showHostPicker || showProfilePicker) {
            Row(
                Modifier.fillMaxWidth().padding(top = 4.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (showHostPicker) {
                    SpawnOptionDropdown(
                        label = "Host",
                        selectedLabel = selectedHost,
                        options = hosts.map { it.name to it.name },
                        onSelect = { selectedHost = it },
                        modifier = Modifier.weight(1f),
                    )
                }
                if (showProfilePicker) {
                    SpawnOptionDropdown(
                        label = "Profile",
                        selectedLabel = selectedProfile,
                        options = profiles.map { it.name to it.name },
                        onSelect = { selectProfile(it) },
                        modifier = Modifier.weight(1f),
                    )
                }
            }
        }
        val showAgentPicker = agents.size > 1
        val modelInfo = agentInfo?.takeIf { it.models.isNotEmpty() }
        if (showAgentPicker || modelInfo != null) {
            Row(
                Modifier.fillMaxWidth().padding(top = 4.dp),
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                if (showAgentPicker) {
                    SpawnOptionDropdown(
                        label = "Provider",
                        selectedLabel = agentInfo?.name ?: selectedAgent,
                        options = agents.map { it.id to it.name },
                        onSelect = { selectedAgent = it },
                        modifier = Modifier.weight(1f),
                    )
                }
                modelInfo?.let { a ->
                    SpawnOptionDropdown(
                        label = "Model",
                        selectedLabel = selectedModel,
                        options = a.models.map { it to it },
                        onSelect = { selectedModel = it },
                        modifier = Modifier.weight(1f),
                    )
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
            onClick = { listing?.path?.let { controller.spawnAt(it, target, spawnHost, selectedAgent, selectedModel, selectedProfile) }; onStarted() },
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

@Composable
private fun SpawnOptionDropdown(
    label: String,
    selectedLabel: String,
    options: List<Pair<String, String>>,
    onSelect: (String) -> Unit,
    modifier: Modifier = Modifier,
) {
    var open by remember { mutableStateOf(false) }
    Box(modifier) {
        OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) {
            Text(
                "$label: ${selectedLabel.ifBlank { "default" }} ▾",
                maxLines = 1,
                overflow = TextOverflow.Ellipsis,
            )
        }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            options.forEach { (value, optionLabel) ->
                DropdownMenuItem(
                    text = { Text(optionLabel, maxLines = 1, overflow = TextOverflow.Ellipsis) },
                    onClick = {
                        onSelect(value)
                        open = false
                    },
                )
            }
        }
    }
}

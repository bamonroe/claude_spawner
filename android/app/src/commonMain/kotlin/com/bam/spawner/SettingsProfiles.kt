package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
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
import com.bam.spawner.net.ProfileInfo
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice the shared Profiles editor (Settings → Profiles) needs. The app-managed
 * execution-profile catalogue is server-persisted, so both clients show and edit the
 * same list; the server broadcasts an updated `profiles` after every change.
 */
interface ProfilesController {
    val connected: StateFlow<Boolean>
    val profiles: StateFlow<List<ProfileInfo>>
    fun putProfile(p: ProfileInfo)
    fun deleteProfile(name: String)
    fun setDefaultProfile(name: String)
}

// Text <-> list/map helpers for the profile editor: lists are one item per line,
// maps are KEY=VALUE per line. Blank lines (and map lines without a '=') are dropped.
private fun linesToList(s: String): List<String> = s.split("\n").map { it.trim() }.filter { it.isNotEmpty() }
private fun listToLines(l: List<String>): String = l.joinToString("\n")
private fun linesToMap(s: String): Map<String, String> = s.split("\n").mapNotNull { line ->
    val t = line.trim()
    val i = t.indexOf('=')
    if (t.isEmpty() || i <= 0) null else t.substring(0, i).trim() to t.substring(i + 1).trim()
}.toMap()
private fun mapToLines(m: Map<String, String>): String = m.entries.joinToString("\n") { "${it.key}=${it.value}" }

/**
 * Settings → Profiles: the app-managed execution-profile catalogue editor. The list
 * and every edit are server-persisted and broadcast, so all clients stay in sync.
 * "Default" is a per-profile marker set from the list rows; a session with no explicit
 * choice runs the default profile.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun ProfilesSettings(controller: ProfilesController, onBack: () -> Unit) {
    val profiles by controller.profiles.collectAsState()
    val connected by controller.connected.collectAsState()

    var name by rememberSaveable { mutableStateOf("") }
    var target by rememberSaveable { mutableStateOf("host") } // "host" | "sandbox"
    var image by rememberSaveable { mutableStateOf("") }
    var homeMount by rememberSaveable { mutableStateOf("") }
    var mounts by rememberSaveable { mutableStateOf("") }
    var creds by rememberSaveable { mutableStateOf("") }
    var env by rememberSaveable { mutableStateOf("") }
    var runArgs by rememberSaveable { mutableStateOf("") }
    var vars by rememberSaveable { mutableStateOf("") }
    var editing by rememberSaveable { mutableStateOf("") } // name being edited, "" = new
    var showForm by rememberSaveable { mutableStateOf(false) }
    val clear = {
        name = ""; target = "host"; image = ""; homeMount = ""; mounts = ""; creds = ""
        env = ""; runArgs = ""; vars = ""; editing = ""
    }

    SettingsScaffold("Profiles", onBack) {
        Text(
            "Execution profiles decide where and how a session's turns run — bare-metal on the host, "
                + "or an isolated sandbox with specific mounts, credentials, and environment. The app owns "
                + "this list; the server stores it and shares it across devices. Lists are one entry per "
                + "line; env and vars are KEY=VALUE per line. {{.Home}}, {{.Session}}, {{.Dir}} and "
                + "{{.Vars.X}} are substituted per turn.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage profiles.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }

        HorizontalDivider()
        Text("Profiles", style = MaterialTheme.typography.titleMedium)
        if (profiles.isEmpty()) {
            Text("None yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (p in profiles) {
            Surface(
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                shape = RoundedCornerShape(12.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Column(Modifier.padding(14.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text(
                                if (p.default) "${p.name}  ·  default" else p.name,
                                style = MaterialTheme.typography.titleMedium,
                            )
                            Text(
                                buildString {
                                    append(if (p.target.isBlank()) "host" else p.target)
                                    if (p.image.isNotBlank()) append("  ·  ${p.image}")
                                },
                                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                            )
                        }
                        TextButton(onClick = {
                            name = p.name; target = if (p.target.isBlank()) "host" else p.target
                            image = p.image; homeMount = p.homeMount
                            mounts = listToLines(p.mounts); creds = listToLines(p.creds)
                            env = mapToLines(p.env); runArgs = listToLines(p.runArgs); vars = mapToLines(p.vars)
                            editing = p.name; showForm = true
                        }) { Text("Edit") }
                        TextButton(onClick = {
                            controller.deleteProfile(p.name)
                            if (editing == p.name) { clear(); showForm = false }
                        }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                    }
                    if (!p.default) {
                        TextButton(onClick = { controller.setDefaultProfile(p.name) }) { Text("Make default") }
                    }
                }
            }
        }

        HorizontalDivider()
        if (!showForm && editing.isBlank()) {
            Button(enabled = connected, onClick = { clear(); showForm = true }) { Text("Add profile") }
        } else {
            Text(if (editing.isBlank()) "Add profile" else "Editing “$editing”", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(name, { name = it }, label = { Text("Name (e.g. sandbox)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Text("Target", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                FilterChip(selected = target == "host", onClick = { target = "host" }, label = { Text("host (bare-metal)") })
                FilterChip(selected = target == "sandbox", onClick = { target = "sandbox" }, label = { Text("sandbox") })
            }
            OutlinedTextField(image, { image = it }, label = { Text("Image (sandbox; blank = server default)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(homeMount, { homeMount = it }, label = { Text("Home mount (blank = none)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(mounts, { mounts = it }, label = { Text("Mounts (one -v spec per line)") }, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(creds, { creds = it }, label = { Text("Credential mounts (one per line)") }, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(env, { env = it }, label = { Text("Env (KEY=VALUE per line)") }, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(runArgs, { runArgs = it }, label = { Text("Run args (one per line)") }, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(vars, { vars = it }, label = { Text("Vars (KEY=VALUE per line)") }, modifier = Modifier.fillMaxWidth())
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && name.isNotBlank(),
                    onClick = {
                        // Preserve the existing default marker when editing (it's set from the list).
                        val wasDefault = profiles.firstOrNull { it.name == editing }?.default ?: false
                        controller.putProfile(
                            ProfileInfo(
                                name = name.trim(), target = target, default = wasDefault,
                                image = image.trim(), homeMount = homeMount.trim(),
                                mounts = linesToList(mounts), creds = linesToList(creds),
                                env = linesToMap(env), runArgs = linesToList(runArgs), vars = linesToMap(vars),
                            ),
                        )
                        clear(); showForm = false
                    },
                ) { Text(if (editing.isBlank()) "Add" else "Save") }
                OutlinedButton(onClick = { clear(); showForm = false }) { Text("Cancel") }
            }
        }
    }
}

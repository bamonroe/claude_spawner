package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material.icons.filled.Star
import androidx.compose.material.icons.filled.StarBorder
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Checkbox
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
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.AgentInfo
import com.bam.spawner.net.Host
import com.bam.spawner.net.Identity
import com.bam.spawner.net.ProfileInfo
import com.bam.spawner.ui.ThemeMode
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice of the app controller the shared Hosts/Identities screens need. The
 * Android [VoiceController] implements it; a future web controller will too. Keeping
 * it small lets these screens live in commonMain and render identically on both
 * clients — and since the server owns the host/identity registries, the two clients
 * show and edit the same data.
 */
interface HostsIdentitiesController {
    val connected: StateFlow<Boolean>
    val hosts: StateFlow<List<Host>>
    val identities: StateFlow<List<Identity>>
    fun requestHosts()
    fun putHost(host: Host)
    fun deleteHost(name: String)
    fun requestIdentities()
    fun createIdentity(name: String, user: String, password: String, genKey: Boolean)
    fun importIdentity(name: String, user: String, password: String, keyPath: String)
    fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String)
    fun deleteIdentity(name: String)
}

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

/**
 * The slice the shared Providers editor (Settings → Providers) needs. The AI
 * backends are compile-time; the app only edits per-backend overrides — the model
 * a fresh spawn defaults to, and which models the voice commands enumerate. Both
 * ride on the server-broadcast `agents` list; a `provider_put` persists a change.
 */
interface ProvidersController {
    val connected: StateFlow<Boolean>
    val agents: StateFlow<List<AgentInfo>>
    fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>)
}

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

@Composable
fun IdentitiesSettings(controller: HostsIdentitiesController, onBack: () -> Unit) {
    val identities by controller.identities.collectAsState()
    val connected by controller.connected.collectAsState()
    val clipboard = LocalClipboardManager.current
    LaunchedEffect(connected) { if (connected) controller.requestIdentities() }

    var newName by rememberSaveable { mutableStateOf("") }
    var newUser by rememberSaveable { mutableStateOf("") }
    var newPassword by rememberSaveable { mutableStateOf("") }
    var genKey by rememberSaveable { mutableStateOf(true) } // generate a keypair (off = password-only)
    var importName by rememberSaveable { mutableStateOf("") }
    var importUser by rememberSaveable { mutableStateOf("") }
    var importPassword by rememberSaveable { mutableStateOf("") }
    var importPath by rememberSaveable { mutableStateOf("") }
    var showForm by rememberSaveable { mutableStateOf(false) } // is the add form expanded?
    var editing by rememberSaveable { mutableStateOf("") }     // identity name being edited, "" = none
    var editUser by rememberSaveable { mutableStateOf("") }
    var editChangePw by rememberSaveable { mutableStateOf(false) }
    var editPassword by rememberSaveable { mutableStateOf("") }
    val clearForm = {
        newName = ""; newUser = ""; newPassword = ""; genKey = true
        importName = ""; importUser = ""; importPassword = ""; importPath = ""; showForm = false
        editing = ""; editUser = ""; editChangePw = false; editPassword = ""
    }

    SettingsScaffold("Identities", onBack) {
        Text(
            "SSH keypairs the server holds. The private key never leaves the server — copy the public "
                + "key onto a host's authorized_keys, then point that host at this identity under Hosts.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage identities.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }

        HorizontalDivider()
        Text("Identities", style = MaterialTheme.typography.titleMedium)
        if (identities.isEmpty()) {
            Text("None yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (id in identities) {
            Surface(
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                shape = RoundedCornerShape(12.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Column(Modifier.padding(14.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(id.name, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium)
                        TextButton(onClick = {
                            clearForm(); editing = id.name; editUser = id.user
                        }) { Text("Edit") }
                        TextButton(onClick = { controller.deleteIdentity(id.name) }) {
                            Text("Delete", color = MaterialTheme.colorScheme.error)
                        }
                    }
                    Text(
                        buildString {
                            append(id.user.ifBlank { "(no user)" })
                            if (id.hasPassword) append("  ·  password")
                            if (id.publicKey.isBlank()) append("  ·  no key")
                        },
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                    )
                    // Public key is safe to show/copy; the private key stays on the server.
                    // A password-only identity has none.
                    if (id.publicKey.isNotBlank()) {
                        Text(
                            id.publicKey,
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.outline,
                            maxLines = 2, overflow = TextOverflow.Ellipsis,
                            modifier = Modifier.padding(top = 4.dp),
                        )
                        OutlinedButton(
                            onClick = { clipboard.setText(AnnotatedString(id.publicKey)) },
                            modifier = Modifier.padding(top = 6.dp),
                        ) { Text("Copy public key") }
                    }
                }
            }
        }

        HorizontalDivider()
        // Editing an identity changes its user/password only — the keypair is kept.
        if (editing.isNotBlank()) {
            Text("Editing “$editing”", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(editUser, { editUser = it }, label = { Text("Username (login user)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Change password", Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                Switch(checked = editChangePw, onCheckedChange = { editChangePw = it })
            }
            if (editChangePw) {
                OutlinedTextField(
                    editPassword, { editPassword = it }, label = { Text("New password (blank clears it)") }, singleLine = true,
                    visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth(),
                )
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && editUser.isNotBlank(),
                    onClick = { controller.updateIdentity(editing, editUser.trim(), editChangePw, editPassword); clearForm() },
                ) { Text("Save") }
                OutlinedButton(onClick = clearForm) { Text("Cancel") }
            }
        } else if (!showForm) {
            // Generate / import forms are collapsed until "Add identity" expands them.
            Button(enabled = connected, onClick = { showForm = true }) { Text("Add identity") }
        } else {
            Text("Generate a new identity", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(newName, { newName = it }, label = { Text("Name (e.g. work-key)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(newUser, { newUser = it }, label = { Text("Username (login user)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(
                newPassword, { newPassword = it }, label = { Text("Password (optional)") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth(),
            )
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Generate a keypair", Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                Switch(checked = genKey, onCheckedChange = { genKey = it })
            }
            // A key-less identity (keypair off) must have a password to authenticate.
            Button(
                enabled = connected && newName.isNotBlank() && newUser.isNotBlank() && (genKey || newPassword.isNotBlank()),
                onClick = { controller.createIdentity(newName.trim(), newUser.trim(), newPassword, genKey); clearForm() },
            ) { Text(if (genKey) "Generate keypair" else "Add identity") }

            HorizontalDivider()
            Text("Import an existing key", style = MaterialTheme.typography.titleMedium)
            Text(
                "Register a private key already on the server (e.g. the key it already uses to connect) so "
                    + "it shows up here and can be linked to hosts.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
            )
            OutlinedTextField(importName, { importName = it }, label = { Text("Name") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(importUser, { importUser = it }, label = { Text("Username (login user)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(importPath, { importPath = it }, label = { Text("Private-key path on server") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(
                importPassword, { importPassword = it }, label = { Text("Password (optional)") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(), modifier = Modifier.fillMaxWidth(),
            )
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && importName.isNotBlank() && importUser.isNotBlank() && importPath.isNotBlank(),
                    onClick = { controller.importIdentity(importName.trim(), importUser.trim(), importPassword, importPath.trim()); clearForm() },
                ) { Text("Import key") }
                OutlinedButton(onClick = clearForm) { Text("Cancel") }
            }
        }
    }
}

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun HostsSettings(controller: HostsIdentitiesController, onBack: () -> Unit) {
    val hosts by controller.hosts.collectAsState()
    val identities by controller.identities.collectAsState()
    val connected by controller.connected.collectAsState()
    // Refresh the host + identity registries whenever we (re)connect while open.
    LaunchedEffect(connected) { if (connected) { controller.requestHosts(); controller.requestIdentities() } }

    // Editor state — empty name means "adding a new host"; loading a row edits it.
    var name by rememberSaveable { mutableStateOf("") }
    var address by rememberSaveable { mutableStateOf("") }
    var user by rememberSaveable { mutableStateOf("") }
    var port by rememberSaveable { mutableStateOf("") }
    var keyFile by rememberSaveable { mutableStateOf("") }
    var identity by rememberSaveable { mutableStateOf("") } // selected identity name, "" = none
    var claudeBin by rememberSaveable { mutableStateOf("") }
    var editing by rememberSaveable { mutableStateOf("") } // name of the host being edited, "" = new
    var showForm by rememberSaveable { mutableStateOf(false) } // is the add/edit form expanded?
    val clear = {
        name = ""; address = ""; user = ""; port = ""; keyFile = ""; identity = ""; claudeBin = ""; editing = ""
    }

    SettingsScaffold("Hosts", onBack) {
        Text(
            "SSH targets a session can run on. The app owns this list; the server stores it so it "
                + "persists and is shared across devices. The address is dialed directly — use a real "
                + "hostname or IP (a Tailscale IP is fine), not an ~/.ssh/config alias.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage hosts.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }

        HorizontalDivider()
        Text("Configured hosts", style = MaterialTheme.typography.titleMedium)
        // localhost (loopback) is seeded by default but is an ordinary entry — edit or
        // delete it like any other. A server that can't reach its own box just removes
        // it and drives remotes only.
        if (hosts.isEmpty()) {
            Text("None yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (h in hosts) {
            Surface(
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                shape = RoundedCornerShape(12.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Row(Modifier.padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) {
                        Text(h.name, style = MaterialTheme.typography.titleMedium)
                        Text(
                            buildString {
                                append(if (h.address.isBlank()) h.name else h.address)
                                if (h.port != 0) append(":${h.port}")
                                if (h.user.isNotBlank()) append("  ·  ${h.user}")
                                if (h.identity.isNotBlank()) append("  ·  ${h.identity}")
                            },
                            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                        )
                    }
                    TextButton(onClick = {
                        name = h.name; address = h.address; user = h.user
                        port = if (h.port != 0) h.port.toString() else ""
                        keyFile = h.keyFile; identity = h.identity; claudeBin = h.claudeBin
                        editing = h.name; showForm = true
                    }) { Text("Edit") }
                    TextButton(onClick = {
                        controller.deleteHost(h.name)
                        if (editing == h.name) { clear(); showForm = false }
                    }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                }
            }
        }

        HorizontalDivider()
        // The add/edit form is collapsed until "Add host" (or an Edit) expands it.
        if (!showForm && editing.isBlank()) {
            Button(enabled = connected, onClick = { clear(); showForm = true }) { Text("Add host") }
        } else {
            Text(if (editing.isBlank()) "Add host" else "Editing “$editing”", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(name, { name = it }, label = { Text("Name (e.g. work)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(address, { address = it }, label = { Text("Address (hostname / IP)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(user, { user = it }, label = { Text("User (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            OutlinedTextField(
                port, { port = it.filter { c -> c.isDigit() } }, label = { Text("Port (optional, 22)") },
                singleLine = true, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number),
                modifier = Modifier.fillMaxWidth(),
            )
            OutlinedTextField(keyFile, { keyFile = it }, label = { Text("Key file path on server (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            // Identity picker: a managed keypair supersedes the key-file path. "None" leaves
            // auth to the key file / ssh-agent.
            Text("Identity (optional — supersedes key file)", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                FilterChip(selected = identity == "", onClick = { identity = "" }, label = { Text("None") })
                identities.forEach { id ->
                    FilterChip(selected = identity == id.name, onClick = { identity = id.name }, label = { Text(id.name) })
                }
            }
            // "claude" here is deliberate, not a default-backend placeholder: the host's
            // `claude_bin` field overrides ONLY the Claude backend's binary on that host
            // (server: SSHPool.binFor). Other backends resolve their binaries from their
            // own config (e.g. SPAWNER_SSH_CODEX_BIN), never from this field.
            OutlinedTextField(claudeBin, { claudeBin = it }, label = { Text("Remote claude binary (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && name.isNotBlank() && address.isNotBlank(),
                    onClick = {
                        controller.putHost(
                            Host(
                                name = name.trim(), address = address.trim(), user = user.trim(),
                                port = port.toIntOrNull() ?: 0, keyFile = keyFile.trim(),
                                identity = identity, claudeBin = claudeBin.trim(),
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

/**
 * Settings → Providers. One card per AI backend: pick the model a fresh spawn
 * defaults to, and toggle which models the voice "list models" / "use model N"
 * commands enumerate. The backends and their catalogues are fixed server-side; a
 * Save writes a `provider_put` and the server re-broadcasts the enriched `agents`.
 */
@Composable
fun ProvidersSettings(controller: ProvidersController, onBack: () -> Unit) {
    val agents by controller.agents.collectAsState()
    val connected by controller.connected.collectAsState()

    SettingsScaffold("Providers", onBack) {
        Text(
            "Providers are the AI backends the server can run (Claude, Codex, opencode). The backends "
                + "and their model lists are fixed on the server — here you choose, per backend, the model "
                + "a new session starts on, and which models the voice “list models” / “use model N” "
                + "commands read out. Hiding a model from voice keeps it in the visual picker.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage providers.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }
        if (agents.isEmpty()) {
            Text("No backends advertised yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (a in agents) {
            HorizontalDivider()
            ProviderCard(a, connected, controller::putProvider)
        }
    }
}

/** One backend's editable settings card (default model + voice-enumerable set). */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun ProviderCard(
    agent: AgentInfo,
    connected: Boolean,
    onSave: (String, String, List<String>) -> Unit,
) {
    // Local edit state, seeded from the server and re-seeded whenever a broadcast
    // changes this backend's saved values (e.g. right after our own Save).
    var selDefault by remember(agent.id) { mutableStateOf(agent.defaultModel) }
    var voiceSel by remember(agent.id) { mutableStateOf(agent.voiceModels.toSet()) }
    LaunchedEffect(agent.defaultModel, agent.voiceModels) {
        selDefault = agent.defaultModel
        voiceSel = agent.voiceModels.toSet()
    }
    val dirty = selDefault != agent.defaultModel || voiceSel != agent.voiceModels.toSet()

    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(14.dp)) {
            Text(agent.name, style = MaterialTheme.typography.titleMedium)
            Text("${agent.id}  ·  ${agent.models.size} models", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

            if (agent.models.isEmpty()) {
                Text("No selectable models.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                return@Column
            }

            Text("Default model", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                for (m in agent.models) {
                    FilterChip(selected = selDefault == m, onClick = { selDefault = m }, label = { Text(m) })
                }
            }

            Text("Enumerated by voice", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                for (m in agent.models) {
                    val on = m in voiceSel
                    FilterChip(
                        selected = on,
                        onClick = { voiceSel = if (on) voiceSel - m else voiceSel + m },
                        label = { Text(m) },
                    )
                }
            }

            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && dirty,
                    onClick = {
                        // Send the voice set in catalogue order so the spoken ordinals are stable.
                        onSave(agent.id, selDefault, agent.models.filter { it in voiceSel })
                    },
                ) { Text("Save") }
                if (dirty) {
                    OutlinedButton(onClick = { selDefault = agent.defaultModel; voiceSel = agent.voiceModels.toSet() }) { Text("Reset") }
                }
            }
        }
    }
}

/** The settings landing screen: a list of rows that open each sub-screen. */
@Composable
fun SettingsHub(onOpen: (String) -> Unit, onBack: () -> Unit) {
    SettingsScaffold("Settings", onBack) {
        SettingsRow("Server", "URL, token, connection") { onOpen("set_server") }
        SettingsRow("Appearance", "Theme") { onOpen("set_appearance") }
        SettingsRow("Commands", "Reference & aliases") { onOpen("set_commands") }
        SettingsRow("Audio", "Mic meter, thresholds, transcription, end token") { onOpen("set_audio") }
        SettingsRow("Hosts", "SSH targets sessions can run on") { onOpen("set_hosts") }
        SettingsRow("Identities", "SSH keypairs hosts authenticate with") { onOpen("set_identities") }
        SettingsRow("Profiles", "How & where sessions run (sandbox, mounts, env)") { onOpen("set_profiles") }
        SettingsRow("Providers", "AI backends: default model & voice model list") { onOpen("set_providers") }
        SettingsRow("Debug", "Hit-zone overlays & gesture logging") { onOpen("set_debug") }
    }
}

/** Debug: developer overlays and verbose gesture logging, all off by default. */
@Composable
fun DebugSettings(settings: Prefs, onBack: () -> Unit) {
    SettingsScaffold("Debug", onBack) {
        var overlays by remember { mutableStateOf(settings.debugOverlays) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Hit-zone overlays & logging", style = MaterialTheme.typography.titleMedium)
                Text("Draw translucent boxes over the normally-invisible push-to-talk zones — " +
                    "drag left past the red box to discard a clip, drag up past the amber box to " +
                    "switch into hands-free — with a live drift readout while you hold. Also logs " +
                    "each hold's end reason and finger drift to logcat (tag PTT).",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = overlays, onCheckedChange = { overlays = it; settings.debugOverlays = it })
        }
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

/** Appearance: theme mode, the per-reply token badge, and the cache-warm timer toggle. */
@Composable
fun AppearanceSettings(settings: Prefs, themeMode: ThemeMode, onThemeChange: (ThemeMode) -> Unit, onBack: () -> Unit) {
    SettingsScaffold("Appearance", onBack) {
        Text("Theme", style = MaterialTheme.typography.titleMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            ThemeChoice("System", themeMode == ThemeMode.SYSTEM) { onThemeChange(ThemeMode.SYSTEM) }
            ThemeChoice("Light", themeMode == ThemeMode.LIGHT) { onThemeChange(ThemeMode.LIGHT) }
            ThemeChoice("Dark", themeMode == ThemeMode.DARK) { onThemeChange(ThemeMode.DARK) }
        }

        HorizontalDivider()
        Text("Token badge", style = MaterialTheme.typography.titleMedium)
        Text("Show each reply's token usage under its bubble. Detailed adds the warm-cache split.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        var badge by remember { mutableStateOf(settings.tokenBadge) }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            ThemeChoice("Off", badge == "off") { badge = "off"; settings.tokenBadge = "off" }
            ThemeChoice("Compact", badge == "compact") { badge = "compact"; settings.tokenBadge = "compact" }
            ThemeChoice("Detailed", badge == "detailed") { badge = "detailed"; settings.tokenBadge = "detailed" }
        }

        HorizontalDivider()
        var warm by remember { mutableStateOf(settings.cacheWarmTimer) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Warm-cache countdown", style = MaterialTheme.typography.titleMedium)
                Text("Display-only: count down the ~5-min window where the next turn reuses a warm prompt cache.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = warm, onCheckedChange = { warm = it; settings.cacheWarmTimer = it })
        }
    }
}

/** A pill button used for exclusive single-choice rows (theme, badge, whisper model). */
@Composable
fun ThemeChoice(label: String, selected: Boolean, onClick: () -> Unit) {
    if (selected) {
        Button(onClick = onClick) { Text(label) }
    } else {
        OutlinedButton(onClick = onClick) { Text(label) }
    }
}

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
    var wakeTok by rememberSaveable { mutableStateOf(settings.wakeToken) }
    var endTok by rememberSaveable { mutableStateOf(settings.endToken) }
    var speakTok by rememberSaveable { mutableStateOf(settings.speakToken) }
    var gate by remember { mutableStateOf(settings.dictationGate) }
    var useDetector by remember { mutableStateOf(settings.wakeService == "detector") }
    var silence by remember { mutableStateOf(if (settings.silenceCommitSeconds <= 0f) "" else settings.silenceCommitSeconds.toString()) }
    SettingsScaffold("Commands", onBack) {
        Text("Say your wake word → a command → your end token.", style = MaterialTheme.typography.bodyMedium)

        HorizontalDivider()
        Text("Wake token", style = MaterialTheme.typography.titleMedium)
        OutlinedTextField(
            wakeTok, { wakeTok = it },
            label = { Text("Custom wake words (blank = \"hey buddy\" only)") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
        )
        OutlinedButton(onClick = { settings.wakeToken = wakeTok.trim(); onSttChanged() }) { Text("Apply wake token") }
        Text(
            "Extra phrase(s) that also open a command, alongside the built-in \"hey buddy\". "
                + "Separate several with commas — handy when whisper mis-hears one. "
                + "Pick words whisper hears cleanly.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

        HorizontalDivider()
        Text("End token", style = MaterialTheme.typography.titleMedium)
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedTextField(endTok, { endTok = it }, label = { Text("Commits a message") }, singleLine = true, modifier = Modifier.weight(1f))
            endTokenTest(endTok)
        }
        OutlinedButton(onClick = { settings.endToken = endTok; onSttChanged() }) { Text("Apply end token") }
        Text("Say this to commit a hands-free message.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

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
        OutlinedTextField(
            speakTok, { speakTok = it },
            label = { Text("Speak token(s), comma-separated") },
            singleLine = true, modifier = Modifier.fillMaxWidth(),
        )
        OutlinedButton(onClick = { settings.speakToken = speakTok.trim(); onSttChanged() }) { Text("Apply speak token") }
        Text(
            "Say this to start dictating, then your end token to send — e.g. \"take a note … beep\". "
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

/**
 * Audio settings: VAD thresholds, TTS toggles, the resident-whisper URL, and the
 * server-global transcription model pair (full + quick) — prefs backed by [Prefs], the models
 * read/pushed live via [controller]. (The end token, wake token, and silence auto-commit live on
 * the Commands page, since they're part of the command grammar; the "brief replies" and "ask
 * before guessing" session-behavior toggles live on the Server page.) [micMeter] is a platform slot
 * that draws the live mic-level bar (Android reads the recorder; web is empty until M5's Web
 * Audio); it receives the current threshold so it can mark it on the bar.
 */
@Composable
fun AudioSettings(
    settings: Prefs,
    controller: AppController,
    onVadChanged: () -> Unit,
    onSttChanged: () -> Unit,
    onBack: () -> Unit,
    micMeter: @Composable (Double) -> Unit = {},
) {
    var threshold by remember { mutableStateOf(settings.vadThreshold.toFloat()) }
    var whisperUrl by remember { mutableStateOf(settings.whisperUrl) }

    SettingsScaffold("Audio", onBack) {
        micMeter(threshold.toDouble())
        Text("Mic threshold (lower = more sensitive): ${threshold.toInt()}", style = MaterialTheme.typography.bodyMedium)
        Slider(
            value = threshold, onValueChange = { threshold = it },
            valueRange = 200f..1500f, steps = 12,
            onValueChangeFinished = { settings.vadThreshold = threshold.toInt(); onVadChanged() },
        )
        VadSlider("Sustained speech to start (ms)", settings.vadOnsetMs, 40, 400, 20) {
            settings.vadOnsetMs = it; onVadChanged()
        }
        VadSlider("Silence to end / \"I'm done\" (ms)", settings.vadSilenceMs, 400, 2000, 100) {
            settings.vadSilenceMs = it; onVadChanged()
        }
        var adaptive by remember { mutableStateOf(settings.vadAdaptive) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Adapt to background noise", style = MaterialTheme.typography.titleMedium)
                Text("Track the room's noise floor and lift the mic threshold above it automatically. The slider above then acts as a minimum.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = adaptive, onCheckedChange = { adaptive = it; settings.vadAdaptive = it; onVadChanged() })
        }
        var headsetNs by remember { mutableStateOf(settings.headsetNoiseSuppression) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Headset noise suppression", style = MaterialTheme.typography.titleMedium)
                Text("Run the phone's noise suppressor on the headset mic path too. Helps steady background noise, but can attenuate far-field voice.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = headsetNs, onCheckedChange = { headsetNs = it; settings.headsetNoiseSuppression = it; onVadChanged() })
        }

        // The hands-free mic source (device vs headset) now lives in the top-bar audio
        // picker's Input section, alongside the output route, so it isn't set here.

        HorizontalDivider()
        var summaryOnly by remember { mutableStateOf(settings.summaryOnlySpeech) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Summary only", style = MaterialTheme.typography.titleMedium)
                Text("Only speak a turn's final result; intermediate steps play a soft beep instead. Say \"hey buddy, summary only\" / \"speak everything\" to toggle by voice.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = summaryOnly, onCheckedChange = { summaryOnly = it; settings.summaryOnlySpeech = it })
        }
        var serverTts by remember { mutableStateOf(settings.serverTts) }
        val ttsAvailable by controller.serverTtsAvailable.collectAsState()
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Server voice", style = MaterialTheme.typography.titleMedium)
                Text(
                    if (ttsAvailable)
                        "Speak with the server's Kokoro voice, streamed to this device. Off (or on any failure) the device's own voice is used."
                    else
                        "This server doesn't offer speech synthesis — the device's own voice is used.",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                )
            }
            Switch(
                checked = serverTts, enabled = ttsAvailable,
                onCheckedChange = { serverTts = it; settings.serverTts = it },
            )
        }
        // The Kokoro voice picker (server-relayed catalogue; empty until the server
        // offers TTS). Client-local: the choice rides each speak request; picking a
        // voice speaks a short preview in it.
        val ttsVoices by controller.ttsVoices.collectAsState()
        val ttsVoiceDefault by controller.ttsVoiceDefault.collectAsState()
        if (ttsAvailable && ttsVoices.isNotEmpty()) {
            var voice by remember { mutableStateOf(settings.ttsVoice) }
            var voicesOpen by remember { mutableStateOf(false) }
            Box {
                OutlinedButton(onClick = { voicesOpen = true }, modifier = Modifier.fillMaxWidth()) {
                    Text("Voice: ${voice.ifBlank { "server default ($ttsVoiceDefault)" }} ▾")
                }
                DropdownMenu(expanded = voicesOpen, onDismissRequest = { voicesOpen = false }) {
                    DropdownMenuItem(
                        text = { Text("server default ($ttsVoiceDefault)") },
                        onClick = {
                            voice = ""; settings.ttsVoice = ""; voicesOpen = false
                            controller.previewTtsVoice("")
                        },
                    )
                    ttsVoices.forEach { v ->
                        DropdownMenuItem(text = { Text(v) }, onClick = {
                            voice = v; settings.ttsVoice = v; voicesOpen = false
                            controller.previewTtsVoice(v) // hear it right away
                        })
                    }
                }
            }
        }

        HorizontalDivider()
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedTextField(
                whisperUrl, { whisperUrl = it; settings.whisperUrl = it },
                label = { Text("Whisper server URL") }, singleLine = true, modifier = Modifier.weight(1f),
            )
            OutlinedButton(onClick = { settings.whisperUrl = whisperUrl; onSttChanged() }) { Text("Apply") }
        }
        Text(
            "A resident whisper server (blank = server default). Resolved on the server host — "
                + "\"localhost:8571\" is the whisper container running alongside it. When set, the "
                + "model there is authoritative (the toggles above are ignored).",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

        HorizontalDivider()
        Text("Transcription models", style = MaterialTheme.typography.titleMedium)
        Text(
            "Server-global — shared by every device, hot-loaded on the whisper containers (a few "
                + "seconds). Pick any English model; one marked ⤓ isn't on the server yet and downloads "
                + "on apply (a big model is a slow first fetch, then it's cached).",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        WhisperModelField(
            "Full transcribe model", controller.whisperModel,
            controller.whisperModels, controller.whisperModelsLocal,
        ) { controller.setWhisperModel(it) }
        WhisperModelField(
            "Quick transcribe model", controller.whisperFastModel,
            controller.whisperModels, controller.whisperModelsLocal,
        ) { controller.setWhisperModel(it, fast = true) }
        WhisperDownloadBanner(controller.whisperDownload)
        Text(
            "Full handles dictation; quick handles the live hands-free draft and end-token "
                + "detection. Quick shows \"none\" when the server has no fast whisper server.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
    }
}

/**
 * One server-global whisper model editor, prefilled from the server-reported [current]
 * (re-synced on any change, even from another device), with an Apply that pushes it via
 * [onApply]. When the server advertises its on-disk models ([options] non-empty) the editor
 * is a dropdown of them; otherwise it falls back to a free-text ggml model name. Re-applying
 * the unchanged name is a deliberate pin: the server skips the redundant hot-load but persists
 * the choice to settings.json, so a model that only came from an env default survives restarts.
 */
@Composable
private fun WhisperModelField(
    label: String,
    current: StateFlow<String>,
    options: StateFlow<List<String>>,
    localOptions: StateFlow<List<String>>,
    onApply: (String) -> Unit,
) {
    val cur by current.collectAsState()
    val models by options.collectAsState()
    val local by localOptions.collectAsState()
    var picked by remember { mutableStateOf(cur) }
    LaunchedEffect(cur) { picked = cur }
    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        if (models.isNotEmpty()) {
            var open by remember { mutableStateOf(false) }
            // A catalogue model not yet on the server's disk shows a ⤓ so the user knows
            // applying it triggers a download rather than an instant hot-load.
            val needsDownload = picked.isNotBlank() && picked !in local
            Box(Modifier.weight(1f)) {
                OutlinedButton(onClick = { open = true }, modifier = Modifier.fillMaxWidth()) {
                    Text("$label: ${picked.ifBlank { "none" }}${if (needsDownload) " ⤓" else ""} ▾")
                }
                DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
                    models.forEach { m ->
                        val label = if (m in local) m else "$m  ⤓ download"
                        DropdownMenuItem(text = { Text(label) }, onClick = { picked = m; open = false })
                    }
                }
            }
        } else {
            OutlinedTextField(
                picked, { picked = it },
                label = { Text(label) }, singleLine = true, modifier = Modifier.weight(1f),
                placeholder = { Text(cur.ifBlank { "none" }) },
            )
        }
        OutlinedButton(
            onClick = { onApply(picked.trim()) },
            enabled = picked.trim().isNotBlank(),
        ) { Text("Apply") }
    }
    Text(
        "current: ${cur.ifBlank { "none" }}",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
}

/**
 * Live banner for an on-demand model download the server runs when a client picks a model that
 * isn't on disk yet. Shows a determinate bar when the size is known, an indeterminate one
 * otherwise, and the error (kept visible) on failure. Renders nothing when no download is active.
 */
@Composable
private fun WhisperDownloadBanner(download: StateFlow<WhisperDownloadInfo?>) {
    val d by download.collectAsState()
    val dl = d ?: return
    Column(Modifier.fillMaxWidth()) {
        if (dl.error.isNotBlank()) {
            Text(
                "Download of ${dl.model} failed: ${dl.error}",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error,
            )
            return
        }
        val detail = if (dl.total > 0) "${mbTenths(dl.received)} / ${mbTenths(dl.total)} MB"
        else "${mbTenths(dl.received)} MB"
        Text(
            "Downloading ${dl.model} — $detail",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (dl.total > 0) {
            LinearProgressIndicator(
                progress = { (dl.received.toFloat() / dl.total.toFloat()).coerceIn(0f, 1f) },
                modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
            )
        } else {
            LinearProgressIndicator(modifier = Modifier.fillMaxWidth().padding(top = 4.dp))
        }
    }
}

/** Formats a byte count as megabytes with one decimal, without String.format (KMP-safe). */
private fun mbTenths(bytes: Long): String {
    val tenths = bytes * 10 / 1_000_000
    return "${tenths / 10}.${tenths % 10}"
}

/** A labeled slider for an integer VAD dial; persists on release via [onChange]. */
@Composable
fun VadSlider(label: String, initial: Int, min: Int, max: Int, step: Int, onChange: (Int) -> Unit) {
    var v by remember { mutableStateOf(initial.toFloat()) }
    val steps = ((max - min) / step - 1).coerceAtLeast(0)
    Column {
        Text("$label: ${v.toInt()}", style = MaterialTheme.typography.bodyMedium)
        Slider(
            value = v,
            onValueChange = { v = it },
            valueRange = min.toFloat()..max.toFloat(),
            steps = steps,
            onValueChangeFinished = { onChange(v.toInt()) },
        )
    }
}

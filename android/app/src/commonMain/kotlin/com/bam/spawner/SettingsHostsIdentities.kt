package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
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
import com.bam.spawner.net.Host
import com.bam.spawner.net.Identity
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

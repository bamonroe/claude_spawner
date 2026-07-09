package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
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
import androidx.compose.ui.unit.sp
import com.bam.spawner.net.Host
import com.bam.spawner.net.Identity
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

/** Common wrapper: back arrow + title over a scrollable column. */
@Composable
fun SettingsScaffold(title: String, onBack: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
    Column(
        Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)
            .systemBarsPadding().imePadding().verticalScroll(rememberScrollState()).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(12.dp),
    ) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            TextButton(onClick = onBack) { Text("←", fontSize = 22.sp) }
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
                            if (id.hasPassword) append("  ·  🔒 password")
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
                                if (h.identity.isNotBlank()) append("  ·  🔑 ${h.identity}")
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
                Text("Cache-warm timer", style = MaterialTheme.typography.titleMedium)
                Text("Count down the ~5-min window where the next turn reuses a warm prompt cache.",
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

/** Commands reference + per-command alias editor (fixes whisper mis-hears). */
@Composable
fun CommandsSettings(settings: Prefs, onAliasesChanged: () -> Unit, onBack: () -> Unit) {
    var aliasMap by remember { mutableStateOf(settings.aliasMap()) }
    SettingsScaffold("Commands", onBack) {
        Text("Say \"hey buddy\" → a command → your end token.", style = MaterialTheme.typography.bodyMedium)
        Text(
            "Add aliases for words whisper mis-hears. Tap an alias bubble to remove it.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        COMMANDS.forEach { cmd ->
            CommandAliasGroup(
                cmd = cmd,
                aliases = aliasMap.filterValues { it == cmd.name }.keys.sorted(),
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

@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun CommandAliasGroup(cmd: Command, aliases: List<String>, onAdd: (String) -> Unit, onRemove: (String) -> Unit) {
    var adding by remember { mutableStateOf(false) }
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.35f),
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) {
                    Text(cmd.name, style = MaterialTheme.typography.titleMedium)
                    Text(cmd.description, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                    Text(
                        "say: " + cmd.aliases.joinToString(" · "),
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                    )
                }
                OutlinedButton(onClick = { adding = true }) { Text("＋") }
            }
            if (aliases.isNotEmpty()) {
                FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    aliases.forEach { AliasChip(it) { onRemove(it) } }
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
 * Server connection settings: URL/token + Save & Connect, the server-global whisper
 * model picker, and the restart button. The mutual-TLS client-certificate import is a
 * platform slot ([certSection]) — Android fills it with a SAF `.p12` picker; the web
 * client leaves it empty (browser mTLS is handled by the user's cert store).
 */
@Composable
fun ServerSettings(
    settings: Prefs,
    controller: AppController,
    onSaveConnect: (String, String) -> Unit,
    onBack: () -> Unit,
    certSection: @Composable ColumnScope.() -> Unit = {},
) {
    var url by rememberSaveable { mutableStateOf(settings.url) }
    var token by rememberSaveable { mutableStateOf(settings.token) }
    // The whisper model is server-global: read the current one, pick a new one, then
    // push it. Re-sync the picker whenever the server reports a change (even one made
    // from another device).
    val current by controller.whisperModel.collectAsState()
    var picked by remember { mutableStateOf(current) }
    LaunchedEffect(current) { picked = current }
    val connected by controller.connected.collectAsState()
    var restartConfirm by remember { mutableStateOf(false) }
    SettingsScaffold("Server", onBack) {
        OutlinedTextField(url, { url = it }, label = { Text("Server URL") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(token, { token = it }, label = { Text("Token") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        Button(onClick = {
            settings.url = url; settings.token = token
            onSaveConnect(url, token)
        }) {
            Text("Save & Connect")
        }
        Text("Client ID: ${settings.clientId}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

        certSection()

        HorizontalDivider()
        Text("Whisper model", style = MaterialTheme.typography.titleMedium)
        Text(
            "Shared by every device — this is the model loaded on the server. Changing it hot-swaps "
                + "it for everyone (a few seconds to load).",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            ThemeChoice("Fast", picked == "small.en") { picked = "small.en" }
            ThemeChoice("Balanced", picked == "medium.en") { picked = "medium.en" }
            ThemeChoice("Accurate", picked == "large-v3") { picked = "large-v3" }
        }
        Text(
            "current: ${current.ifBlank { "unknown" }}" +
                if (picked.isNotBlank() && picked != current) "  →  $picked (not applied)" else "",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        Button(
            onClick = { controller.setWhisperModel(picked) },
            enabled = picked.isNotBlank() && picked != current,
        ) { Text("Apply Whisper Model") }

        HorizontalDivider()
        Text("Restart server", style = MaterialTheme.typography.titleMedium)
        Text(
            "Restarts the server process on your machine — it rebuilds from current code, so this "
                + "picks up server changes. In-flight turns are interrupted; the app reconnects on its own.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        Button(
            onClick = { restartConfirm = true },
            enabled = connected,
            colors = ButtonDefaults.buttonColors(containerColor = MaterialTheme.colorScheme.error),
        ) { Text("Restart Server") }
        if (!connected) {
            Text("Connect first.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
    }
    if (restartConfirm) {
        AlertDialog(
            onDismissRequest = { restartConfirm = false },
            title = { Text("Restart the server?") },
            text = { Text("The server will rebuild and relaunch. Any running turn is interrupted; the app reconnects automatically.") },
            confirmButton = { TextButton(onClick = { restartConfirm = false; controller.restartServer() }) { Text("Restart") } },
            dismissButton = { TextButton(onClick = { restartConfirm = false }) { Text("Cancel") } },
        )
    }
}

package com.bam.spawner

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.provider.OpenableColumns
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.activity.result.contract.ActivityResultContracts.RequestPermission
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.material3.Switch
import androidx.core.content.ContextCompat
import com.bam.spawner.service.VoiceService
import androidx.compose.foundation.background
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.detectHorizontalDragGestures
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.detectVerticalDragGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ColumnScope
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FilterChip
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Slider
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.foundation.layout.Spacer
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.view.WindowCompat
import kotlin.math.log10
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.compose.runtime.mutableStateListOf
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.AskQuestion
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.ServerMsg
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.UsageReport
import com.bam.spawner.net.UsageEstimateInfo
import kotlin.math.roundToInt
import com.bam.spawner.ui.MarkdownText
import com.bam.spawner.ui.SpawnerTheme
import com.bam.spawner.ui.ThemeMode
import com.bam.spawner.ui.parseThemeMode
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

class MainActivity : ComponentActivity() {
    private lateinit var controller: VoiceController
    private lateinit var settings: SettingsStore
    private val micPermission = registerForActivityResult(RequestPermission()) { granted ->
        if (granted && settings.handsFree) startHandsFreeService()
    }
    private val notifPermission = registerForActivityResult(RequestPermission()) {}
    private val btPermission = registerForActivityResult(RequestPermission()) { granted ->
        if (granted) controller.setAudioOutput(AudioOutput.BLUETOOTH)
    }

    /** Route the spoken audio, requesting BLUETOOTH_CONNECT first if Bluetooth is
     *  chosen and not yet granted (needed to route to a Bluetooth headset). */
    private fun selectAudioOutput(out: AudioOutput) {
        if (out == AudioOutput.BLUETOOTH &&
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.BLUETOOTH_CONNECT) != PackageManager.PERMISSION_GRANTED
        ) {
            btPermission.launch(Manifest.permission.BLUETOOTH_CONNECT)
        } else {
            controller.setAudioOutput(out)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // Edge-to-edge so the IME inset is delivered to Compose (imePadding) — the
        // input bar rides just above the keyboard instead of the window panning.
        WindowCompat.setDecorFitsSystemWindows(window, false)
        settings = SettingsStore(this)
        controller = Spawner.controller(this) // shared with VoiceService
        micPermission.launch(Manifest.permission.RECORD_AUDIO)
        controller.connectIfNeeded(settings.url, settings.token) // auto-connect; then auto-reconnects
        if (settings.handsFree) setHandsFree(true) // restore hands-free across restarts
        setContent {
            var mode by remember { mutableStateOf(parseThemeMode(settings.themeMode)) }
            SpawnerTheme(mode) {
                // Surface provides the correct on-background content color (so text is
                // light in dark mode, dark in light mode).
                Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
                    AppRoot(
                        controller, settings, mode,
                        onToggleHandsFree = ::setHandsFree,
                        onSelectAudioOutput = ::selectAudioOutput,
                    ) { newMode ->
                        settings.themeMode = newMode.name.lowercase()
                        mode = newMode
                    }
                }
            }
        }
    }

    // Foreground state drives whether a finished turn posts a notification.
    override fun onResume() {
        super.onResume()
        controller.appForeground = true
    }

    override fun onStop() {
        super.onStop()
        controller.appForeground = false
    }

    /** Toggle always-listening: start/stop the mic foreground service (and perms). */
    private fun setHandsFree(on: Boolean) {
        settings.handsFree = on
        if (on) {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
                notifPermission.launch(Manifest.permission.POST_NOTIFICATIONS)
            }
            if (hasMic()) startHandsFreeService() else micPermission.launch(Manifest.permission.RECORD_AUDIO)
        } else {
            stopService(Intent(this, VoiceService::class.java))
        }
    }

    private fun hasMic() =
        ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED

    private fun startHandsFreeService() =
        ContextCompat.startForegroundService(this, Intent(this, VoiceService::class.java))
    // Controller/service are process-scoped (Spawner); not shut down on Activity destroy.
}

@Composable
private fun AppRoot(
    controller: VoiceController,
    settings: SettingsStore,
    themeMode: ThemeMode,
    onToggleHandsFree: (Boolean) -> Unit,
    onSelectAudioOutput: (AudioOutput) -> Unit,
    onThemeChange: (ThemeMode) -> Unit,
) {
    var screen by rememberSaveable { mutableStateOf("main") }
    // Reconnect re-sends hello (end token / stt / aliases).
    val reconnect = { controller.connect(settings.url, settings.token) }
    // Audio-hardware surface the shared MainScreen needs as plain values (it's kept off
    // the shared AppController): the mic status text and the output picker's state.
    val connected by controller.connected.collectAsStateWithLifecycle()
    val mic by controller.mic.collectAsStateWithLifecycle()
    val audioOutput by controller.audioOutput.collectAsStateWithLifecycle()
    val audioOutputs by controller.audioOutputs.collectAsStateWithLifecycle()
    // System back: settings sub-pages pop to the hub; hub/browse pop to main.
    BackHandler(enabled = screen != "main") {
        screen = if (screen.startsWith("set_")) "settings" else "main"
    }
    when (screen) {
        "settings" -> SettingsHub(onOpen = { screen = it }, onBack = { screen = "main" })
        "set_server" -> ServerSettings(
            settings, controller,
            onSaveConnect = { url, token -> controller.connect(url, token); screen = "settings" },
            onBack = { screen = "settings" },
            certSection = { ServerCertSection(settings) },
        )
        "set_hosts" -> HostsSettings(controller, onBack = { screen = "settings" })
        "set_identities" -> IdentitiesSettings(controller, onBack = { screen = "settings" })
        "set_appearance" -> AppearanceSettings(settings, themeMode, onThemeChange, onBack = { screen = "settings" })
        "set_commands" -> CommandsSettings(settings, onAliasesChanged = reconnect, onBack = { screen = "settings" })
        "set_audio" -> AudioSettings(
            settings,
            onVadChanged = { controller.restartHandsFree() },
            onSttChanged = reconnect,
            onBack = { screen = "settings" },
            micMeter = { threshold ->
                DisposableEffect(Unit) {
                    controller.startMeter()
                    onDispose { controller.stopMeter() }
                }
                val level by controller.micLevel.collectAsStateWithLifecycle()
                Text("Mic level", style = MaterialTheme.typography.titleMedium)
                LevelMeterBar(level, threshold)
            },
            endTokenTest = { endTok ->
                var calibrating by remember { mutableStateOf(false) }
                OutlinedButton(onClick = {
                    settings.endToken = endTok; calibrating = true; controller.startCalibration()
                }) { Text("Test") }
                if (calibrating) CalibrationDialog(controller, endTok) { controller.stopCalibration(); calibrating = false }
            },
        )
        "browse" -> BrowseScreen(
            controller = controller,
            onStarted = { screen = "main" },
            onBack = { screen = "main" },
        )
        else -> MainScreen(
            controller,
            handsFreeInitial = settings.handsFree,
            badgeMode = settings.tokenBadge,
            showCacheTimer = settings.cacheWarmTimer,
            mic = mic,
            audioOutput = audioOutput,
            audioOutputs = audioOutputs,
            onToggleHandsFree = onToggleHandsFree,
            onSelectAudioOutput = onSelectAudioOutput,
            onRefreshOutputs = controller::refreshAudioOutputs,
            onTalkStart = controller::startTalking,
            onTalkStop = controller::stopTalking,
            onTalkCancel = controller::cancelTalking,
            onStopSpeaking = controller::stopSpeaking,
            onOpenSettings = { screen = "settings" },
            onNewSession = { screen = "browse" }, // BrowseScreen lists the chosen host's root on open
            transferButton = { onUploaded ->
                TransferButton(controller = controller, enabled = connected, onUploaded = onUploaded)
            },
        )
    }
}

/** A file the user picked to upload, held while they choose a destination directory.
 *  [DirHost] is shared with the web transfer button (see commonMain/TransferPicker.kt). */
private data class PendingUpload(val name: String, val content: String, val start: DirHost)

/** The 📎 button left of the message box: a menu to upload a phone file onto the
 *  session's host, or download a host file back to the phone — both over the same
 *  authenticated socket. An upload prefills the message box via [onUploaded]. */
@Composable
private fun TransferButton(
    controller: VoiceController,
    enabled: Boolean,
    onUploaded: (String) -> Unit,
) {
    val context = LocalContext.current
    var menu by remember { mutableStateOf(false) }
    // Non-null while the destination-directory picker is open for an upload.
    var pendingUpload by remember { mutableStateOf<PendingUpload?>(null) }
    // Non-null while the download file-picker is open (its browse start point).
    var downloadStart by remember { mutableStateOf<DirHost?>(null) }
    // A finished download's bytes, awaiting a "save as" destination.
    var pendingSave by remember { mutableStateOf<ServerMsg.FileData?>(null) }

    fun start(): DirHost =
        controller.attachedDirHost()?.let { DirHost(it.first, it.second) } ?: DirHost("/", "")

    // Pick a phone file → hold its name + base64 bytes, then open the dest-dir picker.
    val pickFile = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) {
            val bytes = context.contentResolver.openInputStream(uri)?.use { it.readBytes() }
            val name = context.contentResolver
                .query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                ?.use { c -> if (c.moveToFirst()) c.getString(0) else null } ?: "upload.bin"
            if (bytes != null) {
                val b64 = android.util.Base64.encodeToString(bytes, android.util.Base64.NO_WRAP)
                pendingUpload = PendingUpload(name, b64, start())
            }
        }
    }
    // "Save as" destination for a completed download → write the decoded bytes there.
    val saveFile = rememberLauncherForActivityResult(
        ActivityResultContracts.CreateDocument("application/octet-stream"),
    ) { uri ->
        val data = pendingSave
        pendingSave = null
        if (uri != null && data != null) {
            val bytes = android.util.Base64.decode(data.content, android.util.Base64.NO_WRAP)
            context.contentResolver.openOutputStream(uri)?.use { it.write(bytes) }
        }
    }

    // An upload landed: prefill the message box (do NOT send).
    LaunchedEffect(Unit) { controller.fileSaved.collect { onUploaded(it) } }
    // A download's bytes arrived: open "save as" defaulting to the file's name.
    LaunchedEffect(Unit) {
        controller.fileData.collect { fd -> pendingSave = fd; saveFile.launch(fd.name) }
    }

    Box {
        Box(
            Modifier.size(48.dp).clip(CircleShape)
                .background(MaterialTheme.colorScheme.surfaceVariant)
                .clickable(enabled = enabled) { menu = true },
            contentAlignment = Alignment.Center,
        ) { Text("📎", fontSize = 20.sp) }
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(text = { Text("Upload file") }, onClick = {
                menu = false; pickFile.launch(arrayOf("*/*"))
            })
            DropdownMenuItem(text = { Text("Download file") }, onClick = {
                menu = false; downloadStart = start()
            })
        }
    }

    // Upload: choose the destination directory on the session's host, then send.
    pendingUpload?.let { up ->
        TransferPickerDialog(
            controller = controller,
            host = up.start.host,
            startDir = up.start.dir,
            pickFiles = false,
            title = "Upload “${up.name}” to…",
            onPick = { dir ->
                controller.uploadFile(dir, up.name, up.content, up.start.host)
                pendingUpload = null
            },
            onDismiss = { pendingUpload = null },
        )
    }

    // Download: choose a file on the session's host; its bytes come back as file_data.
    downloadStart?.let { s ->
        TransferPickerDialog(
            controller = controller,
            host = s.host,
            startDir = s.dir,
            pickFiles = true,
            title = "Download a file",
            onPick = { path ->
                controller.downloadFile(path, s.host)
                downloadStart = null
            },
            onDismiss = { downloadStart = null },
        )
    }
}

/** The drawer's session list: EVERY Claude session on the machine (discovery),
 * with registry names/attach merged in. Tap to open; ✏️ rename; 🗑 delete. */


/**
 * The mutual-TLS client-certificate importer — the Android-only slot passed to the shared
 * [ServerSettings]. Picks a `.p12` via the Storage Access Framework, copies it into app-private
 * storage, and persists its passphrase (the passphrase is saved as you type, so the next
 * Save & Connect above picks it up). The `.p12` bytes never leave the device.
 */
@Composable
private fun ServerCertSection(settings: SettingsStore) {
    val context = LocalContext.current
    var certName by remember { mutableStateOf(settings.clientCertName) }
    var certPass by rememberSaveable { mutableStateOf(settings.clientCertPass) }
    val certPicker = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri ->
        if (uri != null) {
            val bytes = context.contentResolver.openInputStream(uri)?.use { it.readBytes() }
            if (bytes != null) {
                val name = context.contentResolver
                    .query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                    ?.use { c -> if (c.moveToFirst()) c.getString(0) else null } ?: "client.p12"
                settings.importClientCert(bytes, name)
                certName = name
            }
        }
    }
    HorizontalDivider()
    Text("Client certificate (mTLS)", style = MaterialTheme.typography.titleMedium)
    Text(
        "Optional. If the server requires mutual TLS, import your .p12 client certificate and "
            + "enter its passphrase — the app presents it on top of the token. Only used for "
            + "wss:// servers; leave empty otherwise.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
    Text(
        if (certName.isBlank()) "No certificate imported." else "Imported: $certName",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
    OutlinedTextField(
        certPass, { certPass = it; settings.clientCertPass = it }, label = { Text("Certificate passphrase") },
        singleLine = true, visualTransformation = PasswordVisualTransformation(),
        modifier = Modifier.fillMaxWidth(),
    )
    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        OutlinedButton(onClick = { certPicker.launch(arrayOf("*/*")) }) { Text("Import .p12…") }
        if (certName.isNotBlank()) {
            OutlinedButton(onClick = {
                settings.clearClientCert(); certName = ""; certPass = ""
            }) { Text("Remove") }
        }
    }
    Text(
        "Changes take effect on the next Save & Connect above.",
        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
    )
}

/** Live mic RMS bar with the VAD threshold marked (speech above the line is captured). */
@Composable
private fun LevelMeterBar(level: Double, threshold: Double) {
    val max = 2000.0
    val fill = (level / max).coerceIn(0.0, 1.0).toFloat()
    val markFrac = (threshold / max).coerceIn(0.0, 1.0).toFloat()
    val db = if (level > 1.0) (20 * log10(level / 32768.0)).toInt() else -90
    val above = level >= threshold
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text("$db dB", style = MaterialTheme.typography.labelMedium)
        BoxWithConstraints(
            Modifier.fillMaxWidth().height(26.dp).clip(RoundedCornerShape(6.dp))
                .background(MaterialTheme.colorScheme.surfaceVariant),
        ) {
            Box(
                Modifier.fillMaxHeight().fillMaxWidth(fill)
                    .background(if (above) Color(0xFF4CAF50) else MaterialTheme.colorScheme.outline),
            )
            Box(
                Modifier.fillMaxHeight().width(2.dp).offset(x = maxWidth * markFrac)
                    .background(MaterialTheme.colorScheme.error),
            )
        }
        Text(
            "The red line is your threshold — speech above it is captured, below is ignored as noise.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
    }
}

/** Runs end-token calibration: say the token N times, shows the recognition rate. */
@Composable
private fun CalibrationDialog(controller: VoiceController, token: String, onClose: () -> Unit) {
    val calib by controller.calibration.collectAsStateWithLifecycle()
    AlertDialog(
        onDismissRequest = onClose,
        title = { Text("Calibrate \"$token\"") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                when {
                    calib.active -> Text("Say \"$token\" clearly, a few times…  ${calib.samples.size}/${calib.rounds}")
                    calib.done -> {
                        val n = calib.samples.size
                        val pct = if (n > 0) calib.hits * 100 / n else 0
                        Text(
                            "Recognized ${calib.hits}/$n  ($pct%)",
                            style = MaterialTheme.typography.titleMedium,
                            color = if (pct >= 80) Color(0xFF4CAF50) else if (pct >= 50) Color(0xFFFFB300) else MaterialTheme.colorScheme.error,
                        )
                        if (pct < 80) Text("Try a more distinctive token (e.g. \"over and out\").", style = MaterialTheme.typography.bodySmall)
                    }
                    else -> Text("Starting…")
                }
                if (calib.samples.isNotEmpty()) {
                    Text("Heard:", style = MaterialTheme.typography.labelMedium)
                    calib.samples.takeLast(10).forEach {
                        val ok = it.lowercase().replace(Regex("[,.!?;:\"]"), " ").contains(token.lowercase())
                        Text("${if (ok) "✓" else "✗"}  ${it.ifBlank { "(nothing)" }}", style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        },
        confirmButton = { TextButton(onClick = onClose) { Text(if (calib.active) "Stop" else "Done") } },
    )
}


@Composable
@OptIn(ExperimentalLayoutApi::class)
private fun BrowseScreen(controller: VoiceController, onStarted: () -> Unit, onBack: () -> Unit) {
    val listing by controller.listing.collectAsStateWithLifecycle()
    val hosts by controller.hosts.collectAsStateWithLifecycle()
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
    val agents by controller.agents.collectAsStateWithLifecycle()
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


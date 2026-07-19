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
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AttachFile
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
import androidx.compose.material3.Icon
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.derivedStateOf
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateListOf
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
import androidx.core.view.WindowCompat
import kotlin.math.log10
import androidx.lifecycle.compose.LifecycleStartEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.bam.spawner.audio.AudioInput
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
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    private lateinit var controller: VoiceController
    private lateinit var settings: SettingsStore
    private val micPermission = registerForActivityResult(RequestPermission()) { granted ->
        if (granted && settings.handsFree) startHandsFreeService()
    }
    private val notifPermission = registerForActivityResult(RequestPermission()) {}
    private val btPermission = registerForActivityResult(RequestPermission()) { granted ->
        if (granted) controller.setAudioInput(AudioInput.HEADSET)
    }

    /** Route the spoken audio. */
    private fun selectAudioOutput(out: AudioOutput) = controller.setAudioOutput(out)

    /** Choose the capture mic, requesting BLUETOOTH_CONNECT first when the headset mic
     *  is chosen and not yet granted (needed to grab a Bluetooth headset's mic). */
    private fun selectAudioInput(inp: AudioInput) {
        if (inp == AudioInput.HEADSET &&
            Build.VERSION.SDK_INT >= Build.VERSION_CODES.S &&
            ContextCompat.checkSelfPermission(this, Manifest.permission.BLUETOOTH_CONNECT) != PackageManager.PERMISSION_GRANTED
        ) {
            btPermission.launch(Manifest.permission.BLUETOOTH_CONNECT)
        } else {
            controller.setAudioInput(inp)
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
                        onSelectAudioInput = ::selectAudioInput,
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
    onSelectAudioInput: (AudioInput) -> Unit,
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
    val audioInput by controller.audioInput.collectAsStateWithLifecycle()
    val audioInputs by controller.audioInputs.collectAsStateWithLifecycle()
    // System back: settings sub-pages pop to the hub; hub/browse pop to main.
    BackHandler(enabled = screen != "main") {
        screen = if (screen.startsWith("set_")) "settings" else "main"
    }
    when (screen) {
        "settings" -> SettingsHub(onOpen = { screen = it }, onBack = { screen = "main" })
        "set_server" -> ServerSettings(
            settings, controller,
            onSaveConnect = { url, token -> controller.connect(url, token); screen = "settings" },
            onSttChanged = reconnect,
            onBack = { screen = "settings" },
            caSection = { ServerCaSection(settings, onChanged = reconnect) },
        )
        "set_hosts" -> HostsSettings(controller, onBack = { screen = "settings" })
        "set_identities" -> IdentitiesSettings(controller, onBack = { screen = "settings" })
        "set_profiles" -> ProfilesSettings(controller, onBack = { screen = "settings" })
        "set_providers" -> ProvidersSettings(controller, onBack = { screen = "settings" })
        "set_debug" -> DebugSettings(settings, onBack = { screen = "settings" })
        "set_about" -> AboutSettings(onBack = { screen = "settings" })
        "set_appearance" -> AppearanceSettings(
            settings, themeMode, onThemeChange,
            onBack = { screen = "settings" },
        )
        "set_commands" -> CommandsSettings(
            settings,
            onAliasesChanged = reconnect,
            onSttChanged = reconnect,
            onBack = { screen = "settings" },
            endTokenTest = { endTok ->
                var calibrating by remember { mutableStateOf(false) }
                OutlinedButton(onClick = {
                    settings.endToken = endTok; calibrating = true; controller.startCalibration()
                }) { Text("Test") }
                if (calibrating) CalibrationDialog(controller, endTok) { controller.stopCalibration(); calibrating = false }
            },
        )
        "set_audio" -> AudioSettings(
            settings,
            controller,
            onVadChanged = { controller.restartHandsFree() },
            onSttChanged = reconnect,
            onBack = { screen = "settings" },
            micMeter = { threshold ->
                // Meter only while the app is in the foreground: the standalone meter
                // holds the mic open, so backgrounding with Audio settings on-screen
                // must release it (ON_STOP) and coming back restarts it (ON_START) —
                // a plain DisposableEffect kept polling the mic in the background.
                LifecycleStartEffect(Unit) {
                    controller.startMeter()
                    onStopOrDispose { controller.stopMeter() }
                }
                val level by controller.micLevel.collectAsStateWithLifecycle()
                Text("Mic level", style = MaterialTheme.typography.titleMedium)
                LevelMeterBar(level, threshold)
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
            trayCommandNames = settings.trayCommandNames().toSet(),
            debugOverlays = settings.debugOverlays,
            mic = mic,
            audioOutput = audioOutput,
            audioOutputs = audioOutputs,
            audioInput = audioInput,
            audioInputs = audioInputs,
            onToggleHandsFree = onToggleHandsFree,
            onSelectAudioOutput = onSelectAudioOutput,
            onSelectAudioInput = onSelectAudioInput,
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

/** A batch of files the user picked to upload, held while they choose one destination
 *  directory. Each entry is a display name + base64 bytes; all land in the same [start]
 *  dir. [DirHost] is shared with the web transfer button (see commonMain/TransferPicker.kt). */
private data class PendingUpload(val files: List<UploadFile>, val start: DirHost)
private data class UploadFile(val name: String, val content: String)

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
    // Downloaded files whose bytes have arrived but haven't been "saved as" yet. Android
    // allows only one activity-result "save as" in flight, so multi-file downloads queue
    // here and are drained one dialog at a time.
    val saveQueue = remember { mutableStateListOf<ServerMsg.FileData>() }
    // The file whose "save as" dialog is currently open (the head of the queue in flight).
    var pendingSave by remember { mutableStateOf<ServerMsg.FileData?>(null) }

    fun start(): DirHost =
        controller.attachedDirHost()?.let { DirHost(it.first, it.second) } ?: DirHost("/", "")

    // Pick one or more phone files → hold their names + base64 bytes, then open the
    // dest-dir picker (all selected files upload into the one chosen directory).
    val pickFiles = rememberLauncherForActivityResult(
        ActivityResultContracts.OpenMultipleDocuments(),
    ) { uris ->
        val files = uris.mapNotNull { uri ->
            val bytes = context.contentResolver.openInputStream(uri)?.use { it.readBytes() }
                ?: return@mapNotNull null
            val name = context.contentResolver
                .query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)
                ?.use { c -> if (c.moveToFirst()) c.getString(0) else null } ?: "upload.bin"
            UploadFile(name, android.util.Base64.encodeToString(bytes, android.util.Base64.NO_WRAP))
        }
        if (files.isNotEmpty()) pendingUpload = PendingUpload(files, start())
    }
    // "Save as" destination for the in-flight download → write the decoded bytes there.
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
    // A download's bytes arrived: enqueue for "save as".
    LaunchedEffect(Unit) { controller.fileData.collect { saveQueue.add(it) } }
    // Drain the queue one "save as" at a time: whenever nothing is in flight and files
    // are waiting, pop the next and open its dialog. Clearing pendingSave (on result)
    // re-triggers this to launch the following one.
    LaunchedEffect(pendingSave, saveQueue.size) {
        if (pendingSave == null && saveQueue.isNotEmpty()) {
            val next = saveQueue.removeAt(0)
            pendingSave = next
            saveFile.launch(next.name)
        }
    }

    Box {
        Box(
            Modifier.size(48.dp).clip(CircleShape)
                .background(MaterialTheme.colorScheme.surfaceVariant)
                .clickable(enabled = enabled) { menu = true },
            contentAlignment = Alignment.Center,
        ) { Icon(Icons.Filled.AttachFile, contentDescription = "Transfer a file") }
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(text = { Text("Upload file") }, onClick = {
                menu = false; pickFiles.launch(arrayOf("*/*"))
            })
            DropdownMenuItem(text = { Text("Download file") }, onClick = {
                menu = false; downloadStart = start()
            })
        }
    }

    // Upload: choose the destination directory on the session's host, then send each file.
    pendingUpload?.let { up ->
        val title = up.files.singleOrNull()?.let { "Upload “${it.name}” to…" }
            ?: "Upload ${up.files.size} files to…"
        TransferPickerDialog(
            controller = controller,
            host = up.start.host,
            startDir = up.start.dir,
            pickFiles = false,
            title = title,
            onPick = { dirs ->
                val dir = dirs.first()
                up.files.forEach { controller.uploadFile(dir, it.name, it.content, up.start.host) }
                pendingUpload = null
            },
            onDismiss = { pendingUpload = null },
        )
    }

    // Download: tick one or more files on the session's host; each comes back as file_data.
    downloadStart?.let { s ->
        TransferPickerDialog(
            controller = controller,
            host = s.host,
            startDir = s.dir,
            pickFiles = true,
            title = "Download files",
            onPick = { paths ->
                paths.forEach { controller.downloadFile(it, s.host) }
                downloadStart = null
            },
            onDismiss = { downloadStart = null },
        )
    }
}

/** The drawer's session list: EVERY Claude session on the machine (discovery),
 * with registry names/attach merged in. Tap to open; ✏️ rename; 🗑 delete. */


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


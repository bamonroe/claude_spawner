package com.bam.spawner

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
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
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.foundation.layout.Spacer
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
        )
        "set_hosts" -> HostsSettings(controller, onBack = { screen = "settings" })
        "set_appearance" -> AppearanceSettings(settings, themeMode, onThemeChange, onBack = { screen = "settings" })
        "set_commands" -> CommandsSettings(settings, onAliasesChanged = reconnect, onBack = { screen = "settings" })
        "set_audio" -> AudioSettings(
            settings, controller,
            onVadChanged = { controller.restartHandsFree() },
            onSttChanged = reconnect,
            onBack = { screen = "settings" },
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
            onToggleHandsFree = onToggleHandsFree,
            onSelectAudioOutput = onSelectAudioOutput,
            onOpenSettings = { screen = "settings" },
            onNewSession = { controller.browse(""); screen = "browse" },
        )
    }
}

@Composable
private fun MainScreen(
    controller: VoiceController,
    handsFreeInitial: Boolean,
    badgeMode: String,
    showCacheTimer: Boolean,
    onToggleHandsFree: (Boolean) -> Unit,
    onSelectAudioOutput: (AudioOutput) -> Unit,
    onOpenSettings: () -> Unit,
    onNewSession: () -> Unit,
) {
    val drawerState = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    val focus = LocalFocusManager.current
    // System back closes the open drawer instead of leaving the app.
    BackHandler(enabled = drawerState.isOpen) { scope.launch { drawerState.close() } }
    // Opening the drawer dismisses the keyboard (clearing the input field's focus
    // hides the IME) so it can't overlap the sidebar, and auto-refreshes the session
    // list so it's current every time it's opened. targetValue fires as the open
    // animation begins, not after it settles.
    LaunchedEffect(drawerState.targetValue) {
        if (drawerState.targetValue == DrawerValue.Open) {
            focus.clearFocus()
            controller.discover()
        }
    }

    val status by controller.status.collectAsStateWithLifecycle()
    val connected by controller.connected.collectAsStateWithLifecycle()
    val chat by controller.chat.collectAsStateWithLifecycle()
    val hasMoreHistory by controller.hasMoreHistory.collectAsStateWithLifecycle()
    val scrollTick by controller.scrollTick.collectAsStateWithLifecycle()
    val discovered by controller.discovered.collectAsStateWithLifecycle()
    val discoverError by controller.discoverError.collectAsStateWithLifecycle()
    val attached by controller.attachedName.collectAsStateWithLifecycle()
    val attachedId by controller.attachedId.collectAsStateWithLifecycle()
    // Hoisted dialogs for the drawer's session list.
    var confirmOpen by remember { mutableStateOf<DiscoveredInfo?>(null) }
    var deleteTarget by remember { mutableStateOf<DiscoveredInfo?>(null) }
    var renameTarget by remember { mutableStateOf<DiscoveredInfo?>(null) }
    // Pull-to-refresh on the session list: kick a discover, then drop the spinner
    // when a fresh list lands or after a short cap so it never hangs (discover is
    // fire-and-forget over the socket, and an unchanged list won't re-emit).
    var refreshing by remember { mutableStateOf(false) }
    LaunchedEffect(refreshing) {
        if (!refreshing) return@LaunchedEffect
        controller.discover()
        withTimeoutOrNull(1500) { snapshotFlow { discovered }.drop(1).first() }
        refreshing = false
    }
    val openSession = { d: DiscoveredInfo ->
        controller.adopt(d.sessionId, d.dir); scope.launch { drawerState.close() }; Unit
    }
    val mic by controller.mic.collectAsStateWithLifecycle()
    val voiceState by controller.voiceState.collectAsStateWithLifecycle()
    val audioOutput by controller.audioOutput.collectAsStateWithLifecycle()
    val ask by controller.ask.collectAsStateWithLifecycle()
    val speaking by controller.speaking.collectAsStateWithLifecycle()
    val audioOutputs by controller.audioOutputs.collectAsStateWithLifecycle()
    val pending by controller.pending.collectAsStateWithLifecycle()
    val activity by controller.activity.collectAsStateWithLifecycle()
    val lastUsage by controller.lastTurnUsage.collectAsStateWithLifecycle()
    val rateLimit by controller.rateLimit.collectAsStateWithLifecycle()
    val usageReport by controller.usageReport.collectAsStateWithLifecycle()
    val usageLoading by controller.usageLoading.collectAsStateWithLifecycle()
    val usageEstimate by controller.usageEstimate.collectAsStateWithLifecycle()
    var handsFree by remember { mutableStateOf(handsFreeInitial) }
    // The command tray (swipe up on the message box). Hoisted here so a tap
    // anywhere outside it — the chat, the bars, the text field — can dismiss it.
    var trayOpen by rememberSaveable { mutableStateOf(false) }

    ModalNavigationDrawer(
        drawerState = drawerState,
        // Opened by the ☰ button or a left-edge swipe (a narrow strip on the far
        // left, see below). We keep the drawer's own gestures limited to when it's
        // already open (swipe-to-close) rather than enabling them for the whole
        // content, which would let any horizontal drag across the chat open it.
        gesturesEnabled = drawerState.isOpen,
        drawerContent = {
            ModalDrawerSheet {
                Sidebar(
                    discovered = discovered,
                    discoverError = discoverError,
                    attached = attached,
                    attachedId = attachedId,
                    onNew = { onNewSession(); scope.launch { drawerState.close() } },
                    refreshing = refreshing,
                    onRefresh = { refreshing = true },
                    onOpen = { d -> if (d.active) confirmOpen = d else openSession(d) },
                    onRename = { renameTarget = it },
                    onDelete = { deleteTarget = it },
                    onDetach = { controller.detach() },
                    rateLimit = rateLimit,
                    usageEstimate = usageEstimate,
                    onCheckUsage = { controller.requestUsage(); scope.launch { drawerState.close() } },
                )
            }
        },
    ) {
      Box(Modifier.fillMaxSize()) {
        Column(
            // systemBarsPadding() insets above the status + nav bars; imePadding()
            // lifts the input bar above the keyboard. NOTE: the chat list below must
            // stay the direct weighted child — wrapping it in a SelectionContainer
            // distorted this Column and pushed the input bar off the bottom.
            Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)
                .systemBarsPadding().imePadding()
                // While the command tray is open, a tap that no child consumed (the
                // chat, the bars, empty space) closes it. Only armed while open, so it
                // never touches normal scrolling/tapping. Tray buttons and the text
                // field consume their own taps, so those don't fall through to here.
                .pointerInput(trayOpen) {
                    if (trayOpen) detectTapGestures { trayOpen = false }
                },
        ) {
            TopBar(
                title = attached ?: "Claude Spawner",
                subtitle = status,
                contextTokens = lastUsage?.usage?.contextTokens,
                onMenu = { scope.launch { drawerState.open() } },
                onSettings = onOpenSettings,
                audioOutput = audioOutput,
                audioOutputs = audioOutputs,
                onSelectOutput = onSelectAudioOutput,
                onOutputMenuOpened = controller::refreshAudioOutputs,
            )
            if (attached == null) DetachedBanner()
            // The status bars below the list are Column siblings: showing one shrinks
            // the list from the bottom. ChatList watches its own viewport height and
            // re-pins the newest message above the bars (and the keyboard) itself.
            val showWarmBar = showCacheTimer && lastUsage != null
            ChatList(chat, hasMoreHistory, scrollTick, badgeMode, controller::loadOlder, Modifier.weight(1f).fillMaxWidth())
            if (showWarmBar) lastUsage?.let { CacheWarmBar(it) }
            if (speaking) SpeakingBar(onStop = controller::stopSpeaking)
            if (activity.isNotBlank()) ActivityIndicator(activity, onAbort = controller::abortTurn)
            if (pending.isNotBlank()) DraftLine(pending)
            if (handsFree) VoiceStatePill(voiceState)
            if (mic.isNotEmpty()) {
                Text(
                    mic, color = MaterialTheme.colorScheme.primary,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                    style = MaterialTheme.typography.labelMedium,
                )
            }
            InputBar(
                connected = connected,
                trayOpen = trayOpen,
                onTrayOpenChange = { trayOpen = it },
                // While hands-free owns the mic, push-to-talk is disabled — but the
                // button still accepts a swipe-up to toggle hands-free back off.
                handsFree = handsFree,
                onToggleHandsFree = { on -> handsFree = on; onToggleHandsFree(on) },
                onTalkStart = { controller.startTalking() },
                onTalkStop = { controller.stopTalking() },
                onTalkCancel = { controller.cancelTalking() },
                onSend = { controller.sendText(it) },
            )
        }
        // Left-edge swipe to open the drawer: a narrow strip pinned to the far left
        // edge that opens the drawer on a rightward drag. Kept thin (and on the left,
        // away from the mic button on the right) so it doesn't steal normal touches.
        Box(
            Modifier.align(Alignment.CenterStart)
                .fillMaxHeight()
                .width(24.dp)
                .pointerInput(Unit) {
                    val threshold = 24.dp.toPx()
                    var dx = 0f
                    detectHorizontalDragGestures(
                        onDragStart = { dx = 0f },
                        onHorizontalDrag = { _, delta -> dx += delta },
                        onDragEnd = { if (dx >= threshold) scope.launch { drawerState.open() } },
                    )
                },
        )
      }
    }

    // Interactive-mode questions overlay everything.
    ask?.let { AskDialog(it, onSubmit = controller::submitAnswers, onDismiss = controller::dismissAsk) }

    // --- session-list dialogs (hoisted out of the drawer so they overlay) ---
    confirmOpen?.let { d ->
        AlertDialog(
            onDismissRequest = { confirmOpen = null },
            title = { Text("Live in a terminal") },
            text = {
                Text("An interactive claude is running at:\n\n${d.dir}\n\nOpening + dictating drives " +
                    "the same session and can interleave with your terminal. View/history is safe; " +
                    "avoid dictating to both at once.")
            },
            confirmButton = { TextButton(onClick = { confirmOpen = null; openSession(d) }) { Text("Open anyway") } },
            dismissButton = { TextButton(onClick = { confirmOpen = null }) { Text("Cancel") } },
        )
    }
    deleteTarget?.let { d ->
        if (d.active) {
            AlertDialog(
                onDismissRequest = { deleteTarget = null },
                title = { Text("Live in a terminal") },
                text = { Text("Close the terminal session at ${d.dir} first — a running session can't be deleted.") },
                confirmButton = { TextButton(onClick = { deleteTarget = null }) { Text("OK") } },
            )
        } else {
            AlertDialog(
                onDismissRequest = { deleteTarget = null },
                title = { Text("Delete permanently?") },
                text = {
                    Text("This deletes ALL Claude conversations for:\n\n${d.dir}\n\nEvery session's " +
                        "transcript in this directory is removed from disk for good — this can't be undone.")
                },
                confirmButton = {
                    TextButton(onClick = { controller.deleteDiscovered(d.sessionId); deleteTarget = null }) {
                        Text("Delete", color = MaterialTheme.colorScheme.error)
                    }
                },
                dismissButton = { TextButton(onClick = { deleteTarget = null }) { Text("Cancel") } },
            )
        }
    }
    renameTarget?.let { d ->
        var newName by remember(d) { mutableStateOf(d.name) }
        AlertDialog(
            onDismissRequest = { renameTarget = null },
            title = { Text("Rename session") },
            text = { OutlinedTextField(newName, { newName = it }, singleLine = true, label = { Text("Name") }) },
            confirmButton = {
                TextButton(onClick = {
                    if (newName.isNotBlank()) controller.renameDiscovered(d.sessionId, d.dir, newName)
                    renameTarget = null
                }) { Text("Save") }
            },
            dismissButton = { TextButton(onClick = { renameTarget = null }) { Text("Cancel") } },
        )
    }
    // Usage sheet: opened by "Check usage" (tap) or the "usage" voice command
    // (report arrives unprompted). Shows while loading and once the report lands.
    if (usageLoading || usageReport != null) {
        UsageSheet(
            usageLoading, usageReport, usageEstimate,
            onSet = { controller.setUsageBenchmark() },
            onCalc = { controller.calcUsageMax() },
            onDismiss = { controller.dismissUsage() },
        )
    }
}

// UsageSheet shows the Claude plan's `/usage` report: session and weekly percent-
// used as bars up top, then the full contributing breakdown verbatim. Spinner
// while the server runs /usage. See VoiceController.requestUsage.
@Composable
private fun UsageSheet(
    loading: Boolean, report: UsageReport?, estimate: UsageEstimateInfo?,
    onSet: () -> Unit, onCalc: () -> Unit, onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } },
        title = { Text("Claude usage") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                when {
                    report == null -> Row(verticalAlignment = Alignment.CenterVertically) {
                        CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
                        Spacer(Modifier.width(10.dp))
                        Text("Checking usage…")
                    }
                    report.sessionPct < 0 && report.weekPct < 0 -> // parse failed — show raw
                        Text(report.text.ifBlank { "No usage data." }, style = MaterialTheme.typography.bodySmall)
                    else -> {
                        UsageBar("Session", report.sessionPct, report.sessionReset)
                        Spacer(Modifier.height(12.dp))
                        UsageBar("This week", report.weekPct, report.weekReset)
                        // The running server-wide estimate: what it had drifted to (all
                        // sessions/clients) just before this check snapped it back.
                        estimate?.takeIf { it.calibrated }?.let { e ->
                            Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                            Text("Live estimate (all sessions/clients, drifts each turn)",
                                style = MaterialTheme.typography.labelMedium)
                            Text("Session ~${pctStr(e.sessionEstPct)} · Week ~${pctStr(e.weekEstPct)}",
                                style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.primary)
                            Text("odometer: ${fmtTokL(e.cumTokens)} tokens · +${e.turnsSinceCheck} turns since last check",
                                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        }
                        // Manual two-point rate calibration. "Set" marks the current
                        // odometer/percentages; after burning enough tokens to move a few
                        // whole percent, "Calc" sets tokens-per-percent directly from that
                        // interval — no EMA, so it beats the passive check's rounding bias.
                        Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                        Text("Calibrate max (two-point)", style = MaterialTheme.typography.labelMedium)
                        estimate?.takeIf { it.benchSet }?.let { e ->
                            Text("benchmark: ${pctStr(e.benchSessPct)} session · ${pctStr(e.benchWeekPct)} week · +${fmtTokL(e.tokensSinceSet)} tokens since",
                                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        } ?: Text("no benchmark set — tap Set, burn a few % of tokens, then Calc",
                            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        Row {
                            TextButton(onClick = onSet) { Text("📍 Set") }
                            TextButton(onClick = onCalc) { Text("🧮 Calc max") }
                        }
                        val idx = report.text.indexOf("What's contributing")
                        if (idx >= 0) {
                            Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                            Text(report.text.substring(idx), style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.outline)
                        }
                    }
                }
            }
        },
    )
}

// UsageBar is one labeled percent-used row with a progress bar and reset time.
@Composable
private fun UsageBar(label: String, pct: Int, reset: String) {
    val known = pct in 0..100
    Column {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            Text(label, style = MaterialTheme.typography.titleSmall)
            Text(if (known) "$pct% used" else "—", style = MaterialTheme.typography.titleSmall,
                color = if (pct >= 90) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary)
        }
        if (known) LinearProgressIndicator(
            progress = { pct / 100f },
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
        )
        if (reset.isNotBlank()) Text("resets $reset", style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline)
    }
}

/** Shown when no session is attached: a safe "command mode" — utterances are
 * commands (no "hey buddy" needed) and nothing reaches a Claude session. */
@Composable
private fun DetachedBanner() {
    Surface(color = Color(0xFF2E7D32), contentColor = Color.White, modifier = Modifier.fillMaxWidth()) {
        Text(
            "🔓 Detached — command mode. Speak commands directly (no \"hey buddy\" needed); " +
                "nothing goes to a Claude session.",
            modifier = Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
            style = MaterialTheme.typography.bodySmall,
        )
    }
}

@Composable
private fun TopBar(
    title: String,
    subtitle: String,
    contextTokens: Int?,
    onMenu: () -> Unit,
    onSettings: () -> Unit,
    audioOutput: AudioOutput,
    audioOutputs: List<AudioOutput>,
    onSelectOutput: (AudioOutput) -> Unit,
    onOutputMenuOpened: () -> Unit,
) {
    Surface(tonalElevation = 2.dp) {
        Row(
            Modifier.fillMaxWidth().padding(horizontal = 4.dp, vertical = 2.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            TextButton(onClick = onMenu) { Text("☰", fontSize = 22.sp) }
            Column(Modifier.weight(1f)) {
                Text(title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Text("· $subtitle", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
            }
            // Current context size — the last turn's context tokens (input + cache).
            if (contextTokens != null && contextTokens > 0) Text(
                "🧠 ${fmtTok(contextTokens)}",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.outline,
                modifier = Modifier.padding(horizontal = 6.dp),
            )
            AudioOutputButton(audioOutput, audioOutputs, onSelectOutput, onOutputMenuOpened)
            TextButton(onClick = onSettings) { Text("⚙", fontSize = 20.sp) }
        }
    }
}

/** Top-bar button showing the current spoken-audio output; tap to pick another
 *  (Bluetooth appears only while a headset is connected). */
@Composable
private fun AudioOutputButton(
    current: AudioOutput,
    outputs: List<AudioOutput>,
    onSelect: (AudioOutput) -> Unit,
    onOpened: () -> Unit,
) {
    var open by remember { mutableStateOf(false) }
    Box {
        TextButton(onClick = { onOpened(); open = true }) { Text(current.icon, fontSize = 18.sp) }
        DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
            outputs.forEach { out ->
                DropdownMenuItem(
                    text = { Text("${out.icon}  ${out.label}${if (out == current) "  ✓" else ""}") },
                    onClick = { onSelect(out); open = false },
                )
            }
        }
    }
}

/** Shown while TTS is speaking: a full-width tap target that stops the readout. */
@Composable
private fun SpeakingBar(onStop: () -> Unit) {
    Surface(
        color = MaterialTheme.colorScheme.secondaryContainer,
        shape = RoundedCornerShape(14.dp),
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp).clickable { onStop() },
    ) {
        Text(
            "🔊 Speaking — tap to stop",
            Modifier.padding(horizontal = 12.dp, vertical = 10.dp),
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSecondaryContainer,
        )
    }
}

/** Live "Claude is thinking / editing foo.go" indicator, like a typing bubble. */
@Composable
private fun ActivityIndicator(text: String, onAbort: () -> Unit) {
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp),
        horizontalArrangement = Arrangement.SpaceBetween,
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Surface(
            color = MaterialTheme.colorScheme.surfaceVariant,
            shape = RoundedCornerShape(14.dp),
        ) {
            Text(
                text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp),
                style = MaterialTheme.typography.bodyMedium,
                fontStyle = androidx.compose.ui.text.font.FontStyle.Italic,
            )
        }
        TextButton(onClick = onAbort) { Text("⏹ stop", fontSize = 13.sp) }
    }
}

/** Interactive-mode clarification questions: chips for multiple-choice, text
 *  fields otherwise. Also read aloud, so you can just answer by voice (which
 *  dismisses this). */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun AskDialog(questions: List<AskQuestion>, onSubmit: (String) -> Unit, onDismiss: () -> Unit) {
    val answers = remember(questions) { mutableStateListOf<String>().apply { repeat(questions.size) { add("") } } }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Claude needs input") },
        text = {
            Column(
                Modifier.verticalScroll(rememberScrollState()),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                questions.forEachIndexed { i, q ->
                    Text(q.q, style = MaterialTheme.typography.bodyLarge)
                    if (q.options.isEmpty()) {
                        OutlinedTextField(answers[i], { answers[i] = it }, singleLine = true, modifier = Modifier.fillMaxWidth())
                    } else {
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                            q.options.forEach { opt ->
                                FilterChip(selected = answers[i] == opt, onClick = { answers[i] = opt }, label = { Text(opt) })
                            }
                        }
                    }
                }
                Text("…or just answer out loud.", style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.outline)
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    val text = questions.mapIndexed { i, q ->
                        "Q: ${q.q}\nA: ${answers[i].ifBlank { "(no preference)" }}"
                    }.joinToString("\n\n")
                    onSubmit(text)
                },
                enabled = answers.any { it.isNotBlank() },
            ) { Text("Send") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Dismiss") } },
    )
}

/** The live hands-free draft — captured-but-uncommitted text, shown greyed above
 *  the input bar so you can see what's buffered before you say the end token. */
@Composable
private fun DraftLine(text: String) {
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f),
        shape = RoundedCornerShape(10.dp),
        modifier = Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 2.dp),
    ) {
        Text(
            "✎ $text",
            Modifier.padding(horizontal = 10.dp, vertical = 6.dp),
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant.copy(alpha = 0.8f),
            fontStyle = androidx.compose.ui.text.font.FontStyle.Italic,
        )
    }
}

/** Compact hands-free status pill: Listening / Capturing / Thinking / Speaking. */
@Composable
private fun VoiceStatePill(state: VoiceState) {
    val (label, dot) = when (state) {
        VoiceState.OFF -> return
        VoiceState.LISTENING -> "listening for \"hey buddy\"" to Color(0xFF4CAF50)
        VoiceState.CAPTURING -> "listening to you…" to Color(0xFF2196F3)
        VoiceState.THINKING -> "thinking…" to Color(0xFFFFB300)
        VoiceState.SPEAKING -> "speaking… (talk to interrupt)" to Color(0xFF9C27B0)
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 12.dp, vertical = 2.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(Modifier.size(8.dp).background(dot, CircleShape))
        Text(
            "  $label", style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    }
}

@Composable
private fun ChatList(
    messages: List<ChatMessage>,
    hasMore: Boolean,
    scrollTick: Int,
    badgeMode: String,
    onLoadOlder: () -> Unit,
    modifier: Modifier,
) {
    val listState = rememberLazyListState()
    val scope = rememberCoroutineScope()
    // Bottom item index accounts for the "load older" header (item 0) when present,
    // so we land on the actual newest message, not one above it.
    val bottom = (messages.size - 1 + if (hasMore) 1 else 0).coerceAtLeast(0)
    // `pinned` tracks whether the reader is parked at the very bottom — the END of the
    // newest message is actually in view. It gates the auto-follow: if you've scrolled
    // up to read earlier messages, a new message must NOT yank you back down. It is set
    // only while the viewport is stable (see the resize block below) so an append or a
    // keyboard resize can't corrupt it.
    var pinned by remember { mutableStateOf(true) }
    // Auto-scroll to the newest message on append — but only when pinned. Keyed on the
    // LAST message so paging OLDER messages in (which doesn't change the last one) never
    // yanks the view to the bottom.
    val last = messages.lastOrNull()
    LaunchedEffect(last) {
        if (messages.isNotEmpty() && pinned) listState.animateScrollToItem(bottom)
    }
    // Explicit scroll-to-bottom (attach, typed send, read-last). Always follows, and
    // re-pins so subsequent appends resume auto-following.
    LaunchedEffect(scrollTick) {
        if (scrollTick > 0 && messages.isNotEmpty()) {
            pinned = true
            listState.animateScrollToItem(bottom)
        }
    }
    // Keep the newest message pinned above whatever sits below the list — the soft
    // keyboard and the status bars (speaking / activity / draft / mic / warm). They
    // all shrink this weighted list from the bottom (the keyboard via the outer
    // Column's imePadding() under adjustResize), and a LazyColumn does NOT follow its
    // own shrinking viewport, so the tail of the last message would slide out of view.
    //
    // We watch the viewport HEIGHT and, whenever it changes, re-pin to the newest
    // message — but only if we were parked at the bottom BEFORE the resize. Sampling
    // "am I at the bottom" AFTER the shrink is too late: a big shrink (the keyboard is
    // ~40% of the screen) has already pushed the last item out of view, so it would
    // read false and we'd wrongly skip the follow. `pinned` is therefore updated only
    // while the viewport is stable (a genuine scroll), and merely consulted — not
    // overwritten — on a resize. Snap (not animate) so the pin rides the keyboard.
    //
    // "At the bottom" here means the END of the newest message is actually visible, not
    // merely that the last item has scrolled into range — so scrolling up even a little
    // to read earlier text unpins and stops the auto-follow.
    LaunchedEffect(bottom) {
        var lastViewportH = -1
        snapshotFlow {
            val info = listState.layoutInfo
            val lastItem = info.visibleItemsInfo.lastOrNull()
            val atBottom = lastItem != null && lastItem.index >= bottom &&
                lastItem.offset + lastItem.size <= info.viewportEndOffset + 4
            info.viewportSize.height to atBottom
        }.collect { (viewportH, atBottom) ->
            if (viewportH == lastViewportH) {
                pinned = atBottom                                  // stable viewport → real scroll position
            } else {
                // Scroll to the END of the content, not the top of the last item.
                // scrollToItem(bottom, 0) aligns the item's TOP to the viewport top,
                // which for a message TALLER than the (keyboard-shrunk) viewport hides
                // its bottom half behind the keyboard. A large scrollOffset clamps to
                // max scroll, so the item's BOTTOM sits just above the keyboard/bars
                // regardless of the message's height.
                if (pinned && messages.isNotEmpty()) listState.scrollToItem(bottom, Int.MAX_VALUE)
                lastViewportH = viewportH                          // adopt the new height, keep the prior pin
            }
        }
    }
    // LazyColumn is the direct weighted child (wrapping it in a SelectionContainer
    // distorted the Column's height and pushed the input bar off-screen). Selection
    // is per-bubble instead — long-press a message to select/copy it. The Box only
    // overlays a jump-to-latest button; it keeps the same weight the LazyColumn had.
    Box(modifier) {
        LazyColumn(Modifier.fillMaxSize(), state = listState) {
            if (hasMore) item {
                TextButton(onClick = onLoadOlder, modifier = Modifier.fillMaxWidth()) {
                    Text("⤒ load older messages")
                }
            }
            items(messages) { Bubble(it, badgeMode) }
        }
        // When scrolled up, offer a one-tap jump back to the newest message. Sits at
        // the bottom of the chat area, just above the status bars / input bar.
        AnimatedVisibility(
            visible = !pinned && messages.isNotEmpty(),
            modifier = Modifier.align(Alignment.BottomCenter).padding(bottom = 8.dp),
        ) {
            Surface(
                onClick = {
                    pinned = true
                    scope.launch { listState.animateScrollToItem(bottom) }
                },
                shape = CircleShape,
                color = MaterialTheme.colorScheme.primary,
                contentColor = MaterialTheme.colorScheme.onPrimary,
                shadowElevation = 4.dp,
                modifier = Modifier.size(36.dp),
            ) {
                Box(contentAlignment = Alignment.Center) { Text("↓", fontSize = 20.sp) }
            }
        }
    }
}

@Composable
private fun Bubble(msg: ChatMessage, badgeMode: String = "off") {
    val user = msg.role == Role.USER
    val dark = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val bg = when (msg.role) {
        Role.USER -> MaterialTheme.colorScheme.primary
        Role.CLAUDE -> MaterialTheme.colorScheme.surfaceVariant
        Role.SYSTEM -> if (dark) Color(0xFF9A5B12) else Color(0xFFFFE0B2) // amber — "bud", not Claude
    }
    val fg = when (msg.role) {
        Role.USER -> MaterialTheme.colorScheme.onPrimary
        Role.CLAUDE -> MaterialTheme.colorScheme.onSurfaceVariant
        Role.SYSTEM -> if (dark) Color.White else Color(0xFF7A4A00)
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp),
        horizontalArrangement = if (user) Arrangement.End else Arrangement.Start,
    ) {
        Surface(color = bg, contentColor = fg, shape = RoundedCornerShape(14.dp), modifier = Modifier.widthIn(max = 320.dp)) {
            Column {
                // Per-bubble selection so the text is long-press copyable, without a
                // list-wide SelectionContainer (which distorted the Column layout).
                SelectionContainer {
                    if (msg.role == Role.CLAUDE) {
                        MarkdownText(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp))
                    } else {
                        Text(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp), style = MaterialTheme.typography.bodyMedium)
                    }
                }
                // Per-turn token badge under Claude replies (Appearance → Token badge).
                if (msg.role == Role.CLAUDE && badgeMode != "off") msg.usage?.let { TokenBadge(it, badgeMode) }
                // Date/time badge below the token line: bottom-right for Claude, bottom-left
                // for user input. Only live messages carry a timestamp (history has ts=0).
                if (msg.ts > 0) Text(
                    fmtStamp(msg.ts),
                    style = MaterialTheme.typography.labelSmall,
                    color = fg.copy(alpha = 0.5f),
                    modifier = Modifier
                        .align(if (user) Alignment.End else Alignment.Start)
                        .padding(start = 12.dp, end = 12.dp, bottom = 6.dp),
                )
            }
        }
    }
}

// TokenBadge renders one turn's token usage as a small caption under a reply. The
// ⚡ marks a warm prompt-cache hit; detailed mode breaks the context into fresh
// input / cached / newly-cached tokens. See docs/protocol.md's `output.usage`.
@Composable
private fun TokenBadge(u: TokenUsage, mode: String) {
    val label = if (mode == "detailed") buildString {
        append("${fmtTok(u.input)} in")
        if (u.cacheRead > 0) append(" · ${fmtTok(u.cacheRead)} cached")
        if (u.cacheWrite > 0) append(" · ${fmtTok(u.cacheWrite)} new")
        append(" · ${fmtTok(u.output)} out")
        if (u.warmHit) append(" ⚡")
    } else {
        "${fmtTok(u.contextTokens)}↑ ${fmtTok(u.output)}↓" + if (u.warmHit) " ⚡" else ""
    }
    Text(
        label,
        style = MaterialTheme.typography.labelSmall,
        color = LocalContentColor.current.copy(alpha = 0.6f),
        modifier = Modifier.padding(start = 12.dp, end = 12.dp, bottom = 6.dp),
    )
}

// fmtStamp formats a unix-seconds timestamp as a compact local date/time badge.
private fun fmtStamp(unixSeconds: Long): String =
    java.text.SimpleDateFormat("MMM d, h:mm a", java.util.Locale.getDefault())
        .format(java.util.Date(unixSeconds * 1000))

// fmtTok renders a token count compactly: 800, 1.2k, 24k.
private fun fmtTok(n: Int): String = when {
    n >= 10_000 -> "${(n + 500) / 1000}k"
    n >= 1_000 -> "%.1fk".format(n / 1000.0)
    else -> n.toString()
}

// CacheWarmBar counts down the ~5-minute window in which the next turn reuses the
// warm prompt cache (a cache_read hit) rather than rebuilding context. Driven off
// the last turn's completion time; ticks once a second. See Appearance settings.
@Composable
private fun CacheWarmBar(info: TurnUsageInfo) {
    val windowMs = 5 * 60 * 1000L
    var now by remember { mutableStateOf(SystemClock.elapsedRealtime()) }
    LaunchedEffect(info) {
        while (true) {
            now = SystemClock.elapsedRealtime()
            kotlinx.coroutines.delay(1000)
        }
    }
    val remaining = (windowMs - (now - info.atElapsedMs)).coerceAtLeast(0)
    val warm = remaining > 0
    val label = if (warm) {
        "⚡ cache warm · %d:%02d left".format(remaining / 60000, (remaining % 60000) / 1000)
    } else {
        "❄ cache cold — next turn rebuilds context"
    }
    Text(
        label,
        color = if (warm) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outline,
        style = MaterialTheme.typography.labelMedium,
        modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
    )
}

@Composable
private fun InputBar(
    connected: Boolean,
    trayOpen: Boolean,
    onTrayOpenChange: (Boolean) -> Unit,
    handsFree: Boolean,
    onToggleHandsFree: (Boolean) -> Unit,
    onTalkStart: () -> Unit,
    onTalkStop: () -> Unit,
    onTalkCancel: () -> Unit,
    onSend: (String) -> Unit,
) {
    var draft by rememberSaveable { mutableStateOf("") }
    var talking by remember { mutableStateOf(false) }
    // Swipe up on the text box to reveal the argument-free "hey buddy" commands
    // as tappable buttons; a command tap fires it and hides the tray again. The
    // open flag is hoisted so a tap outside the tray can dismiss it (see caller).
    // Non-null while the mic is held: 0f..1f progress of the drag toward the
    // hands-free threshold. Drives the drag track's fill so you can see how far
    // is left. Null hides the track.
    var swipeFraction by remember { mutableStateOf<Float?>(null) }
    val hasText = draft.isNotBlank()
    // While hands-free owns the mic, push-to-talk is disabled.
    val pushToTalkEnabled = !handsFree
    val micLive = connected && pushToTalkEnabled
    Column(Modifier.fillMaxWidth()) {
      AnimatedVisibility(visible = trayOpen) {
        CommandTray(
            connected = connected,
            onCommand = { phrase -> onSend(phrase); onTrayOpenChange(false) },
        )
      }
      Row(
        Modifier.fillMaxWidth().padding(8.dp),
        verticalAlignment = Alignment.Bottom,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        OutlinedTextField(
            value = draft, onValueChange = { draft = it },
            placeholder = { Text("Message…") }, singleLine = false, maxLines = 6,
            // Swipe up to open the command tray, swipe down to close it. Taps still
            // fall through to focus the field (a tap never crosses the drag slop).
            // Any touch on the box while the tray is open dismisses it — observed on
            // the Initial pass without consuming, so the tap still positions the
            // cursor and the swipe-open still works (that handler is armed only when
            // the tray is already open). onFocusChanged covers a first-tap focus; this
            // covers a tap when the swipe-to-open already left the box focused.
            modifier = Modifier.weight(1f)
                .onFocusChanged { if (it.isFocused) onTrayOpenChange(false) }
                .pointerInput(trayOpen) {
                    if (trayOpen) awaitEachGesture {
                        awaitFirstDown(requireUnconsumed = false, pass = PointerEventPass.Initial)
                        onTrayOpenChange(false)
                    }
                }
                .pointerInput(Unit) {
                    val threshold = 32.dp.toPx()
                    var dy = 0f
                    detectVerticalDragGestures(
                        onDragStart = { dy = 0f },
                        onVerticalDrag = { _, delta -> dy += delta },
                        onDragEnd = {
                            if (dy <= -threshold) onTrayOpenChange(true)
                            else if (dy >= threshold) onTrayOpenChange(false)
                        },
                    )
                },
        )
        // One button, WhatsApp-style: SEND when there's text (tap to send, hold to
        // clear); MIC when the box is empty (hold to talk; drag up the track to
        // switch to hands-free); HEADSET when hands-free is on (tap to turn off).
        // The upward drag distance to switch into hands-free — shared so the visual
        // track is exactly as long as the finger must actually travel.
        val swipeUpDp = 120.dp
        val trackWidth = 36.dp // 75% of the 48dp button
        Box(contentAlignment = Alignment.BottomCenter) {
            // The drag track: only visible while the mic is held. It shows the
            // path (and how far) you must drag up to switch into hands-free, and
            // fills toward the headset target as you go.
            swipeFraction?.let { frac ->
                Box(
                    Modifier
                        .offset(y = (-54).dp) // float just above the mic button
                        .size(width = trackWidth, height = swipeUpDp)
                        .clip(RoundedCornerShape(trackWidth / 2))
                        .background(MaterialTheme.colorScheme.surfaceVariant),
                    contentAlignment = Alignment.BottomCenter,
                ) {
                    // Fill grows from the bottom up as the drag nears the threshold.
                    Box(
                        Modifier
                            .fillMaxWidth()
                            .fillMaxHeight(frac)
                            .background(MaterialTheme.colorScheme.primary),
                    )
                    // The target at the top of the track.
                    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.TopCenter) {
                        Text("🎧", fontSize = 12.sp, modifier = Modifier.padding(top = 3.dp))
                    }
                }
            }
            Surface(
                color = when {
                    talking -> MaterialTheme.colorScheme.error
                    handsFree -> MaterialTheme.colorScheme.error // hands-free = live mic; red headset
                    hasText && connected -> MaterialTheme.colorScheme.primary
                    micLive -> MaterialTheme.colorScheme.primary
                    else -> MaterialTheme.colorScheme.surfaceVariant
                },
                shape = CircleShape,
                // Re-arm the gesture whenever the role changes.
                modifier = Modifier.size(48.dp).pointerInput(hasText, handsFree, connected) {
                    // Distance the finger must travel upward for a hold to be
                    // reinterpreted as switching into hands-free instead of push-to-talk.
                    // Deliberately long so a small drift never trips it.
                    val swipeUpPx = swipeUpDp.toPx()
                    when {
                        hasText -> detectTapGestures(
                            onTap = { if (connected) { onSend(draft); draft = "" } },
                            onLongPress = { draft = "" }, // hold clears the box
                        )
                        // Hands-free on: a single tap on the headset turns it off.
                        handsFree -> detectTapGestures(onTap = { onToggleHandsFree(false) })
                        // Empty box + connected + hands-free off: hold to talk, and
                        // drag up past the track to switch into hands-free.
                        connected -> awaitEachGesture {
                            val down = awaitFirstDown(requireUnconsumed = false)
                            down.consume()
                            val startX = down.position.x
                            val startY = down.position.y
                            talking = true; onTalkStart()
                            swipeFraction = 0f // reveal the track
                            var toggled = false
                            var cancelled = false
                            while (true) {
                                val event = awaitPointerEvent()
                                val change = event.changes.firstOrNull { it.id == down.id } ?: break
                                // Own the gesture: consuming keeps a parent (scroll /
                                // swipe-up tray) from stealing it when the finger drifts
                                // off the small button, so we hold the recording until an
                                // actual finger-lift no matter how far the finger wanders.
                                change.consume()
                                if (!change.pressed) break // released
                                // Drift left the full track distance = throw the clip away.
                                val dx = (startX - change.position.x).coerceAtLeast(0f)
                                if (!cancelled && dx >= swipeUpPx) {
                                    cancelled = true
                                    if (talking) { onTalkCancel(); talking = false }
                                    break // discarded; nothing is sent or transcribed
                                }
                                val dy = (startY - change.position.y).coerceAtLeast(0f)
                                swipeFraction = (dy / swipeUpPx).coerceIn(0f, 1f)
                                if (!toggled && dy >= swipeUpPx) {
                                    toggled = true
                                    // Abandon the in-progress push-to-talk; this hold is a switch.
                                    if (talking) { onTalkCancel(); talking = false }
                                }
                            }
                            swipeFraction = null // hide the track
                            when {
                                cancelled -> {} // discarded — nothing sent, nothing transcribed
                                toggled -> onToggleHandsFree(true)
                                talking -> { onTalkStop(); talking = false }
                            }
                        }
                        else -> {} // disconnected: inert
                    }
                },
            ) {
                Box(contentAlignment = Alignment.Center) {
                    Text(
                        when { hasText -> "➤"; !pushToTalkEnabled -> "🎧"; else -> "🎤" },
                        fontSize = 20.sp,
                    )
                }
            }
        }
      }
    }
}

/** The command tray: the argument-free "hey buddy" commands as tap buttons,
 * revealed by swiping up on the message box. Each tap fires the command (with
 * the wake prefix, so the server treats it as a control command even while
 * attached) and the caller hides the tray. Derived from COMMANDS, so it never
 * drifts from the server grammar — commands whose aliases take an argument
 * (a <name>/<dir> placeholder) are excluded since a button can't supply one. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun CommandTray(connected: Boolean, onCommand: (String) -> Unit) {
    val trayCommands = remember { COMMANDS.filter { c -> c.aliases.none { it.contains("<") } } }
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f),
        modifier = Modifier.fillMaxWidth(),
    ) {
        FlowRow(
            Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 10.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            trayCommands.forEach { cmd ->
                OutlinedButton(
                    enabled = connected,
                    onClick = { onCommand("hey buddy " + cmd.aliases.first()) },
                ) { Text(cmd.name) }
            }
        }
    }
}

/** The drawer's session list: EVERY Claude session on the machine (discovery),
 * with registry names/attach merged in. Tap to open; ✏️ rename; 🗑 delete. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun Sidebar(
    discovered: List<DiscoveredInfo>,
    discoverError: String,
    attached: String?,
    attachedId: String,
    onNew: () -> Unit,
    refreshing: Boolean,
    onRefresh: () -> Unit,
    onOpen: (DiscoveredInfo) -> Unit,
    onRename: (DiscoveredInfo) -> Unit,
    onDelete: (DiscoveredInfo) -> Unit,
    onDetach: () -> Unit,
    rateLimit: RateLimitInfo?,
    usageEstimate: UsageEstimateInfo?,
    onCheckUsage: () -> Unit,
) {
    Column(Modifier.fillMaxHeight().statusBarsPadding().navigationBarsPadding().padding(12.dp)) {
        Text("Sessions", style = MaterialTheme.typography.titleLarge)
        Row {
            TextButton(onClick = onNew) { Text("＋ New") }
        }
        if (discoverError.isNotBlank()) {
            Text("⚠️ $discoverError", color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall)
        }
        HorizontalDivider()
        // Pull down anywhere on the list to refresh; it also auto-refreshes on open.
        PullToRefreshBox(
            isRefreshing = refreshing,
            onRefresh = onRefresh,
            modifier = Modifier.weight(1f),
        ) {
        LazyColumn(Modifier.fillMaxSize()) {
            items(discovered) { d ->
                // Highlight the attached row by stable id, not name — the same session
                // can be named differently here than when we attached (e.g. server switch).
                val isAttached = d.registered && attachedId.isNotEmpty() && d.sessionId == attachedId
                Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f).clickable { onOpen(d) }) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            if (d.busy) Text("⚙️ ") else if (d.active) Text("⚠️ ")
                            Text(d.name, style = MaterialTheme.typography.titleSmall,
                                color = if (isAttached) MaterialTheme.colorScheme.primary else Color.Unspecified,
                                fontWeight = if (isAttached) FontWeight.Bold else null)
                            if (d.target == "sandbox") Text("📦 sandbox",
                                Modifier.padding(start = 6.dp),
                                style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.tertiary)
                        }
                        Text(d.dir, style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.outline, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        val parts = mutableListOf<String>()
                        if (d.busy) parts.add("working…")
                        if (isAttached) parts.add("attached")
                        if (d.active) parts.add("live in terminal")
                        else relativeTime(d.lastActive).let { if (it.isNotEmpty()) parts.add(it) }
                        if (parts.isNotEmpty()) Text(parts.joinToString(" · "),
                            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                    }
                    Text("✏️", Modifier.clickable { onRename(d) }.padding(8.dp))
                    Text("🗑", Modifier.clickable { onDelete(d) }.padding(8.dp))
                }
                HorizontalDivider()
            }
        }
        }
        if (attached != null) {
            HorizontalDivider()
            TextButton(onClick = onDetach) { Text("Detach from $attached") }
        }
        // Usage readouts pinned to the bottom of the drawer: the drift-live estimate
        // (nudges each turn, snaps on /usage), the coarse session-limit reset, and
        // "Check usage" to run `/usage` on demand for the exact numbers.
        HorizontalDivider()
        usageEstimate?.takeIf { it.calibrated }?.let { UsageEstimateLine(it) }
        rateLimit?.let { SessionLimitFooter(it) }
        TextButton(onClick = onCheckUsage) { Text("📊 Check usage") }
    }
}

// UsageEstimateLine shows the server-global drift-live estimate — the running
// session/weekly % that nudges up each turn and snaps to real on /usage.
@Composable
private fun UsageEstimateLine(e: UsageEstimateInfo) {
    Row(Modifier.padding(vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
        Text("📊", style = MaterialTheme.typography.labelMedium)
        Spacer(Modifier.width(4.dp))
        Text(
            "Session ~${pctStr(e.sessionEstPct)} · Week ~${pctStr(e.weekEstPct)} (est)",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.primary,
        )
    }
}

/** Percent as "47%", or "—" when unknown (−1). */
private fun pctStr(p: Double): String = if (p < 0) "—" else "${p.roundToInt()}%"

/** Compact token count for large sums: 800, 1.2k, 24k, 3.4M. */
private fun fmtTokL(n: Long): String = when {
    n >= 10_000_000 -> "${(n + 500_000) / 1_000_000}M"
    n >= 1_000_000 -> "%.1fM".format(n / 1_000_000.0)
    n >= 10_000 -> "${(n + 500) / 1000}k"
    n >= 1_000 -> "%.1fk".format(n / 1000.0)
    else -> n.toString()
}

// SessionLimitFooter shows the Claude subscription's usage-window state: which
// window is binding and when it resets, amber if the status has left "allowed".
// The reset time is exact; the status is coarse (no precise remaining quota
// exists). Fed by the `rate_limit` message; see docs/protocol.md.
@Composable
private fun SessionLimitFooter(info: RateLimitInfo) {
    val warn = !info.allowed
    val window = when {
        info.limitType == "five_hour" -> "5-hour session"
        info.limitType.contains("week") -> "weekly"
        info.limitType.isBlank() -> "usage"
        else -> info.limitType
    }
    val reset = if (info.resetsAt > 0) {
        val clock = android.text.format.DateFormat.getTimeFormat(LocalContext.current)
            .format(java.util.Date(info.resetsAt * 1000))
        "resets $clock${relResetSuffix(info.resetsAt)}"
    } else ""
    Column(Modifier.padding(vertical = 4.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(if (warn) "⚠️" else "⏳", style = MaterialTheme.typography.labelMedium)
            Spacer(Modifier.width(4.dp))
            Text(
                "Claude $window limit",
                style = MaterialTheme.typography.labelMedium,
                color = if (warn) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
                fontWeight = if (warn) FontWeight.Bold else null,
            )
        }
        if (reset.isNotEmpty()) Text(reset, style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline)
        if (warn && info.status.isNotBlank()) Text("status: ${info.status}",
            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.error)
        if (info.usingOverage) Text("using overage credits",
            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
    }
}

/** "· in 2h 13m" until a future unix-seconds reset (empty if past/now). */
private fun relResetSuffix(unixSeconds: Long): String {
    val secs = unixSeconds - System.currentTimeMillis() / 1000
    if (secs <= 0) return ""
    val h = secs / 3600; val m = (secs % 3600) / 60
    return when {
        h > 0 -> " · in ${h}h ${m}m"
        m > 0 -> " · in ${m}m"
        else -> " · soon"
    }
}

/** Coarse "2h ago" / "3d ago" from a unix-seconds timestamp. */
private fun relativeTime(unixSeconds: Long): String {
    if (unixSeconds <= 0) return ""
    val secs = System.currentTimeMillis() / 1000 - unixSeconds
    return when {
        secs < 60 -> "just now"
        secs < 3600 -> "${secs / 60}m ago"
        secs < 86400 -> "${secs / 3600}h ago"
        secs < 86400 * 30 -> "${secs / 86400}d ago"
        else -> "${secs / (86400 * 30)}mo ago"
    }
}

/** Common wrapper: back arrow + title over a scrollable column. */
@Composable
private fun SettingsScaffold(title: String, onBack: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
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
private fun SettingsHub(onOpen: (String) -> Unit, onBack: () -> Unit) {
    SettingsScaffold("Settings", onBack) {
        SettingsRow("Server", "URL, token, connection") { onOpen("set_server") }
        SettingsRow("Appearance", "Theme") { onOpen("set_appearance") }
        SettingsRow("Commands", "Reference & aliases") { onOpen("set_commands") }
        SettingsRow("Audio", "Mic meter, thresholds, transcription, end token") { onOpen("set_audio") }
        SettingsRow("Hosts", "SSH targets sessions can run on") { onOpen("set_hosts") }
    }
}

@Composable
private fun SettingsRow(title: String, subtitle: String, onClick: () -> Unit) {
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

// The loopback host name. To the server, localhost is just another SSH host —
// dialed over loopback SSH using the server's SSH defaults — not a special implicit
// default, so the app always names it explicitly and lists it like any other host.
// A deployment whose server can't reach its own box simply never picks Local.
const val LOCAL_HOST = "localhost"

@Composable
private fun HostsSettings(controller: VoiceController, onBack: () -> Unit) {
    val hosts by controller.hosts.collectAsStateWithLifecycle()
    val connected by controller.connected.collectAsStateWithLifecycle()
    // Refresh the registry whenever we (re)connect while this screen is open.
    LaunchedEffect(connected) { if (connected) controller.requestHosts() }

    // Editor state — empty name means "adding a new host"; loading a row edits it.
    var name by rememberSaveable { mutableStateOf("") }
    var address by rememberSaveable { mutableStateOf("") }
    var user by rememberSaveable { mutableStateOf("") }
    var port by rememberSaveable { mutableStateOf("") }
    var keyFile by rememberSaveable { mutableStateOf("") }
    var claudeBin by rememberSaveable { mutableStateOf("") }
    var editing by rememberSaveable { mutableStateOf("") } // name of the host being edited, "" = new
    val clear = {
        name = ""; address = ""; user = ""; port = ""; keyFile = ""; claudeBin = ""; editing = ""
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
        OutlinedTextField(claudeBin, { claudeBin = it }, label = { Text("Remote claude binary (optional)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Button(
                enabled = connected && name.isNotBlank() && address.isNotBlank(),
                onClick = {
                    controller.putHost(
                        com.bam.spawner.net.Host(
                            name = name.trim(), address = address.trim(), user = user.trim(),
                            port = port.toIntOrNull() ?: 0, keyFile = keyFile.trim(), claudeBin = claudeBin.trim(),
                        ),
                    )
                    clear()
                },
            ) { Text(if (editing.isBlank()) "Add" else "Save") }
            if (name.isNotBlank() || editing.isNotBlank()) {
                OutlinedButton(onClick = clear) { Text("Clear") }
            }
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
                            },
                            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                        )
                    }
                    TextButton(onClick = {
                        name = h.name; address = h.address; user = h.user
                        port = if (h.port != 0) h.port.toString() else ""
                        keyFile = h.keyFile; claudeBin = h.claudeBin; editing = h.name
                    }) { Text("Edit") }
                    TextButton(onClick = {
                        controller.deleteHost(h.name)
                        if (editing == h.name) clear()
                    }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                }
            }
        }
    }
}

@Composable
private fun ServerSettings(
    settings: SettingsStore,
    controller: VoiceController,
    onSaveConnect: (String, String) -> Unit,
    onBack: () -> Unit,
) {
    var url by rememberSaveable { mutableStateOf(settings.url) }
    var token by rememberSaveable { mutableStateOf(settings.token) }
    // Client certificate (mutual TLS): pick a .p12 via the Storage Access
    // Framework, copy it into private storage, and remember its passphrase.
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
    // The whisper model is server-global: read the current one, pick a new one,
    // then push it. Re-sync the picker whenever the server reports a change (even
    // one made from another device).
    val current by controller.whisperModel.collectAsStateWithLifecycle()
    var picked by remember { mutableStateOf(current) }
    LaunchedEffect(current) { picked = current }
    val connected by controller.connected.collectAsStateWithLifecycle()
    var restartConfirm by remember { mutableStateOf(false) }
    SettingsScaffold("Server", onBack) {
        OutlinedTextField(url, { url = it }, label = { Text("Server URL") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(token, { token = it }, label = { Text("Token") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        Button(onClick = {
            settings.url = url; settings.token = token
            settings.clientCertPass = certPass
            onSaveConnect(url, token)
        }) {
            Text("Save & Connect")
        }
        Text("Client ID: ${settings.clientId}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

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
            certPass, { certPass = it }, label = { Text("Certificate passphrase") },
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

@Composable
private fun AppearanceSettings(settings: SettingsStore, themeMode: ThemeMode, onThemeChange: (ThemeMode) -> Unit, onBack: () -> Unit) {
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

// The `Command` type and the alphabetical `COMMANDS` list are GENERATED at build
// time from docs/commands.json (see the generateCommands Gradle task), whose
// source of truth is the server's command registry. Don't hand-maintain a list
// here — add commands in the server registry so the app can never drift.

@Composable
private fun CommandsSettings(settings: SettingsStore, onAliasesChanged: () -> Unit, onBack: () -> Unit) {
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

@Composable
private fun AudioSettings(
    settings: SettingsStore,
    controller: VoiceController,
    onVadChanged: () -> Unit,
    onSttChanged: () -> Unit,
    onBack: () -> Unit,
) {
    DisposableEffect(Unit) {
        controller.startMeter()
        onDispose { controller.stopMeter() }
    }
    val level by controller.micLevel.collectAsStateWithLifecycle()
    var threshold by remember { mutableStateOf(settings.vadThreshold.toFloat()) }
    var endTok by rememberSaveable { mutableStateOf(settings.endToken) }
    var calibrating by remember { mutableStateOf(false) }
    var silence by remember { mutableStateOf(if (settings.silenceCommitSeconds <= 0f) "" else settings.silenceCommitSeconds.toString()) }
    var whisperUrl by remember { mutableStateOf(settings.whisperUrl) }

    SettingsScaffold("Audio", onBack) {
        Text("Mic level", style = MaterialTheme.typography.titleMedium)
        LevelMeterBar(level, threshold.toDouble())
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

        HorizontalDivider()
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
        Text("End token", style = MaterialTheme.typography.titleMedium)
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            OutlinedTextField(endTok, { endTok = it }, label = { Text("Commits a message") }, singleLine = true, modifier = Modifier.weight(1f))
            OutlinedButton(onClick = { settings.endToken = endTok; calibrating = true; controller.startCalibration() }) { Text("Test") }
        }
        if (calibrating) CalibrationDialog(controller, endTok) { controller.stopCalibration(); calibrating = false }
        OutlinedButton(onClick = { settings.endToken = endTok; onSttChanged() }) { Text("Apply end token") }
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
    }
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

/** A labeled slider for an integer VAD dial; persists on release via [onChange]. */
@Composable
private fun VadSlider(label: String, initial: Int, min: Int, max: Int, step: Int, onChange: (Int) -> Unit) {
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

@Composable
@OptIn(ExperimentalLayoutApi::class)
private fun BrowseScreen(controller: VoiceController, onStarted: () -> Unit, onBack: () -> Unit) {
    val listing by controller.listing.collectAsStateWithLifecycle()
    val hosts by controller.hosts.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { controller.browse(""); controller.requestHosts() } // roots + host list on open
    val atRoots = listing?.path.isNullOrEmpty()
    var newFolder by remember { mutableStateOf<String?>(null) } // non-null = the New-folder dialog is open
    var sandbox by remember { mutableStateOf(false) } // execution target: host (default) vs sandbox
    var selectedHost by rememberSaveable { mutableStateOf(LOCAL_HOST) } // an explicit host name (LOCAL_HOST = loopback)
    // Keep the pick valid as the registry loads: if the current host isn't in the list
    // (e.g. localhost was deleted), fall back to the first configured host.
    LaunchedEffect(hosts) {
        if (hosts.isNotEmpty() && hosts.none { it.name == selectedHost }) selectedHost = hosts.first().name
    }
    val target = if (sandbox) "sandbox" else "host"
    // A host only applies to the host target (a sandbox runs locally); drop any
    // selection when switching to sandbox so we never send a stale host.
    val spawnHost = if (sandbox) "" else selectedHost

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
                    onClick = { controller.spawnNewFolder(parent, newFolder!!, target, spawnHost); newFolder = null; onStarted() },
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
        Text(
            if (atRoots) "Pick a location" else (listing?.path ?: ""),
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.outline,
            maxLines = 1, overflow = TextOverflow.Ellipsis,
            modifier = Modifier.padding(vertical = 4.dp),
        )
        HorizontalDivider()

        LazyColumn(Modifier.weight(1f)) {
            if (!atRoots) {
                item {
                    Row(
                        Modifier.fillMaxWidth().clickable { controller.browse(listing?.parent ?: "") }.padding(vertical = 12.dp),
                    ) { Text("⬆  ..") }
                }
            }
            items(listing?.entries ?: emptyList()) { e ->
                Row(
                    Modifier.fillMaxWidth().clickable { controller.browse(e.path) }.padding(vertical = 12.dp),
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
        Row(
            Modifier.fillMaxWidth().padding(top = 8.dp),
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
        Button(
            onClick = { listing?.path?.let { controller.spawnAt(it, target, spawnHost) }; onStarted() },
            enabled = !atRoots,
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
        ) { Text(if (atRoots) "Choose a folder…" else "Start session here") }
        OutlinedButton(
            onClick = { newFolder = "" },
            enabled = !atRoots, // need a location under a root to create inside
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
        ) { Text("New project folder here…") }
    }
}

@Composable
private fun ThemeChoice(label: String, selected: Boolean, onClick: () -> Unit) {
    if (selected) {
        Button(onClick = onClick) { Text(label) }
    } else {
        OutlinedButton(onClick = onClick) { Text(label) }
    }
}

package com.bam.spawner

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts.RequestPermission
import androidx.compose.material3.Switch
import androidx.core.content.ContextCompat
import com.bam.spawner.service.VoiceService
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.detectTapGestures
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
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.HorizontalDivider
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
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.view.WindowCompat
import kotlin.math.log10
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.SessionInfo
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
                    AppRoot(controller, settings, mode, onToggleHandsFree = ::setHandsFree) { newMode ->
                        settings.themeMode = newMode.name.lowercase()
                        mode = newMode
                    }
                }
            }
        }
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
    onThemeChange: (ThemeMode) -> Unit,
) {
    var screen by rememberSaveable { mutableStateOf("main") }
    // Reconnect re-sends hello (end token / stt / aliases).
    val reconnect = { controller.connect(settings.url, settings.token) }
    when (screen) {
        "settings" -> SettingsHub(onOpen = { screen = it }, onBack = { screen = "main" })
        "set_server" -> ServerSettings(
            settings,
            onSaveConnect = { url, token -> controller.connect(url, token); screen = "settings" },
            onBack = { screen = "settings" },
        )
        "set_appearance" -> AppearanceSettings(themeMode, onThemeChange, onBack = { screen = "settings" })
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
        "discover" -> DiscoverScreen(
            controller = controller,
            onAdopted = { screen = "main" },
            onBack = { screen = "main" },
        )
        else -> MainScreen(
            controller,
            handsFreeInitial = settings.handsFree,
            onToggleHandsFree = onToggleHandsFree,
            onOpenSettings = { screen = "settings" },
            onNewSession = { controller.browse(""); screen = "browse" },
            onDiscover = { controller.discover(); screen = "discover" },
        )
    }
}

@Composable
private fun MainScreen(
    controller: VoiceController,
    handsFreeInitial: Boolean,
    onToggleHandsFree: (Boolean) -> Unit,
    onOpenSettings: () -> Unit,
    onNewSession: () -> Unit,
    onDiscover: () -> Unit,
) {
    val drawerState = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()

    val status by controller.status.collectAsStateWithLifecycle()
    val connected by controller.connected.collectAsStateWithLifecycle()
    val chat by controller.chat.collectAsStateWithLifecycle()
    val hasMoreHistory by controller.hasMoreHistory.collectAsStateWithLifecycle()
    val scrollTick by controller.scrollTick.collectAsStateWithLifecycle()
    val sessions by controller.sessions.collectAsStateWithLifecycle()
    val attached by controller.attachedName.collectAsStateWithLifecycle()
    val mic by controller.mic.collectAsStateWithLifecycle()
    val voiceState by controller.voiceState.collectAsStateWithLifecycle()
    val pending by controller.pending.collectAsStateWithLifecycle()
    val activity by controller.activity.collectAsStateWithLifecycle()
    var handsFree by remember { mutableStateOf(handsFreeInitial) }

    ModalNavigationDrawer(
        drawerState = drawerState,
        // Open via the ☰ button, not edge-swipe — an edge swipe would conflict with
        // holding the mic button (bottom-left) to talk.
        gesturesEnabled = drawerState.isOpen,
        drawerContent = {
            ModalDrawerSheet {
                Sidebar(
                    sessions = sessions,
                    attached = attached,
                    onNew = { onNewSession(); scope.launch { drawerState.close() } },
                    onDiscover = { onDiscover(); scope.launch { drawerState.close() } },
                    onRefresh = { controller.refreshSessions() },
                    onAttach = { controller.attachTo(it); scope.launch { drawerState.close() } },
                    onRename = { old, new -> controller.renameSession(old, new) },
                    onDelete = { controller.deleteSession(it) },
                    onDetach = { controller.detach() },
                )
            }
        },
    ) {
        Column(
            // systemBarsPadding() insets above the status + nav bars; imePadding()
            // lifts the input bar above the keyboard. NOTE: the chat list below must
            // stay the direct weighted child — wrapping it in a SelectionContainer
            // distorted this Column and pushed the input bar off the bottom.
            Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background)
                .systemBarsPadding().imePadding(),
        ) {
            TopBar(
                title = attached ?: "Claude Spawner",
                subtitle = status,
                handsFree = handsFree,
                onToggleHandsFree = { on -> handsFree = on; onToggleHandsFree(on) },
                onMenu = { scope.launch { drawerState.open() } },
                onSettings = onOpenSettings,
            )
            ChatList(chat, hasMoreHistory, scrollTick, controller::loadOlder, Modifier.weight(1f).fillMaxWidth())
            if (activity.isNotBlank()) ActivityIndicator(activity)
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
                // While hands-free owns the mic, push-to-talk is disabled.
                pushToTalkEnabled = !handsFree,
                onTalkStart = { controller.startTalking() },
                onTalkStop = { controller.stopTalking() },
                onSend = { controller.sendText(it) },
            )
        }
    }
}

@Composable
private fun TopBar(
    title: String,
    subtitle: String,
    handsFree: Boolean,
    onToggleHandsFree: (Boolean) -> Unit,
    onMenu: () -> Unit,
    onSettings: () -> Unit,
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
            Text("🎧", fontSize = 15.sp)
            Switch(checked = handsFree, onCheckedChange = onToggleHandsFree)
            TextButton(onClick = onSettings) { Text("⚙", fontSize = 20.sp) }
        }
    }
}

/** Live "Claude is thinking / editing foo.go" indicator, like a typing bubble. */
@Composable
private fun ActivityIndicator(text: String) {
    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp), horizontalArrangement = Arrangement.Start) {
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
    }
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
    onLoadOlder: () -> Unit,
    modifier: Modifier,
) {
    val listState = rememberLazyListState()
    // Auto-scroll to the newest message on append. Keyed on the LAST message so
    // paging OLDER messages in (which doesn't change the last one) never yanks the
    // view to the bottom.
    val last = messages.lastOrNull()
    LaunchedEffect(last) {
        if (messages.isNotEmpty()) listState.animateScrollToItem(messages.size - 1)
    }
    // read-last: force-scroll to the bottom so the re-read messages are in view.
    LaunchedEffect(scrollTick) {
        if (scrollTick > 0 && messages.isNotEmpty()) listState.animateScrollToItem(messages.size - 1)
    }
    // LazyColumn is the direct weighted child (wrapping it in a SelectionContainer
    // distorted the Column's height and pushed the input bar off-screen). Selection
    // is per-bubble instead — long-press a message to select/copy it.
    LazyColumn(modifier, state = listState) {
        if (hasMore) item {
            TextButton(onClick = onLoadOlder, modifier = Modifier.fillMaxWidth()) {
                Text("⤒ load older messages")
            }
        }
        items(messages) { Bubble(it) }
    }
}

@Composable
private fun Bubble(msg: ChatMessage) {
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
            // Per-bubble selection so the text is long-press copyable, without a
            // list-wide SelectionContainer (which distorted the Column layout).
            SelectionContainer {
                if (msg.role == Role.CLAUDE) {
                    MarkdownText(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp))
                } else {
                    Text(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp), style = MaterialTheme.typography.bodyMedium)
                }
            }
        }
    }
}

@Composable
private fun InputBar(
    connected: Boolean,
    pushToTalkEnabled: Boolean,
    onTalkStart: () -> Unit,
    onTalkStop: () -> Unit,
    onSend: (String) -> Unit,
) {
    var draft by rememberSaveable { mutableStateOf("") }
    var talking by remember { mutableStateOf(false) }
    val micLive = connected && pushToTalkEnabled
    Row(
        Modifier.fillMaxWidth().padding(8.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        Surface(
            color = if (talking) MaterialTheme.colorScheme.error
            else if (micLive) MaterialTheme.colorScheme.primary
            else MaterialTheme.colorScheme.surfaceVariant,
            shape = CircleShape,
            // pointerInput keyed on micLive so it re-arms when hands-free toggles.
            modifier = Modifier.size(48.dp).pointerInput(micLive) {
                if (!micLive) return@pointerInput
                detectTapGestures(onPress = {
                    talking = true; onTalkStart(); tryAwaitRelease(); onTalkStop(); talking = false
                })
            },
        ) { Box(contentAlignment = Alignment.Center) { Text(if (pushToTalkEnabled) "🎤" else "🎧") } }

        OutlinedTextField(
            value = draft, onValueChange = { draft = it },
            placeholder = { Text("Message…") }, singleLine = true,
            modifier = Modifier.weight(1f),
        )
        Button(onClick = { onSend(draft); draft = "" }, enabled = connected && draft.isNotBlank()) { Text("Send") }
    }
}

@Composable
private fun Sidebar(
    sessions: List<SessionInfo>,
    attached: String?,
    onNew: () -> Unit,
    onDiscover: () -> Unit,
    onRefresh: () -> Unit,
    onAttach: (String) -> Unit,
    onRename: (String, String) -> Unit,
    onDelete: (String) -> Unit,
    onDetach: () -> Unit,
) {
    Column(Modifier.fillMaxHeight().statusBarsPadding().padding(12.dp)) {
        Text("Sessions", style = MaterialTheme.typography.titleLarge)
        Row {
            TextButton(onClick = onNew) { Text("＋ New") }
            TextButton(onClick = onDiscover) { Text("🔍 Discover") }
            TextButton(onClick = onRefresh) { Text("⟳") }
        }
        HorizontalDivider()
        LazyColumn(Modifier.weight(1f)) {
            items(sessions) { s ->
                SessionRow(s, attached == s.name, { onAttach(s.name) }, { onRename(s.name, it) }, { onDelete(s.name) })
            }
        }
        if (attached != null) {
            HorizontalDivider()
            TextButton(onClick = onDetach) { Text("Detach from $attached") }
        }
    }
}

/** Lists all Claude sessions found on disk; tap one to adopt + open it. */
@Composable
private fun DiscoverScreen(controller: VoiceController, onAdopted: () -> Unit, onBack: () -> Unit) {
    val discovered by controller.discovered.collectAsStateWithLifecycle()
    val discoverError by controller.discoverError.collectAsStateWithLifecycle()
    var confirm by remember { mutableStateOf<DiscoveredInfo?>(null) }
    var deleteTarget by remember { mutableStateOf<DiscoveredInfo?>(null) }
    val open = { d: DiscoveredInfo -> controller.adopt(d.sessionId, d.dir); onAdopted() }

    SettingsScaffold("Discover sessions", onBack) {
        Text(
            "Claude sessions found on this machine — tap to open one in the app. " +
                "⚠️ means a live session is open in a terminal; dictating to it from here can conflict.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (discoverError.isNotBlank()) {
            Text("⚠️ $discoverError", color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodySmall, modifier = Modifier.padding(top = 6.dp))
        }
        if (discovered.isEmpty()) {
            Text("Scanning… (or none found)", style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.outline, modifier = Modifier.padding(top = 12.dp))
        }
        // A plain Column (not LazyColumn) because SettingsScaffold already wraps
        // its content in a verticalScroll — nesting a LazyColumn in that crashes.
        Column(Modifier.fillMaxWidth()) {
            discovered.forEach { d ->
                Row(Modifier.fillMaxWidth().padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f).clickable { if (d.active) confirm = d else open(d) }) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            if (d.active) Text("⚠️ ", style = MaterialTheme.typography.bodyMedium)
                            Text(d.dir.substringAfterLast('/').ifEmpty { d.dir },
                                style = MaterialTheme.typography.titleSmall)
                            if (d.registered) Text("  · in app", style = MaterialTheme.typography.labelSmall,
                                color = MaterialTheme.colorScheme.primary)
                        }
                        Text(d.dir, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                            maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Text(relativeTime(d.lastActive) + if (d.active) " · live in terminal" else "",
                            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                    }
                    TextButton(onClick = { deleteTarget = d }) { Text("🗑") }
                }
                HorizontalDivider()
            }
        }
    }

    confirm?.let { d ->
        AlertDialog(
            onDismissRequest = { confirm = null },
            title = { Text("Live in a terminal") },
            text = {
                Text("An interactive claude is running at:\n\n${d.dir}\n\n" +
                    "Opening + dictating from the app drives the same session and can interleave " +
                    "with your terminal. View/history is safe; avoid dictating to both at once.")
            },
            confirmButton = { TextButton(onClick = { confirm = null; open(d) }) { Text("Open anyway") } },
            dismissButton = { TextButton(onClick = { confirm = null }) { Text("Cancel") } },
        )
    }

    deleteTarget?.let { d ->
        if (d.active) {
            AlertDialog(
                onDismissRequest = { deleteTarget = null },
                title = { Text("Live in a terminal") },
                text = { Text("Close the terminal session at ${d.dir} first — a session that's currently running can't be deleted.") },
                confirmButton = { TextButton(onClick = { deleteTarget = null }) { Text("OK") } },
            )
        } else {
            AlertDialog(
                onDismissRequest = { deleteTarget = null },
                title = { Text("Delete permanently?") },
                text = {
                    Text("This deletes ALL Claude conversations for:\n\n${d.dir}\n\n" +
                        "Every session's transcript in this directory is removed from disk for good — " +
                        "this can't be undone.")
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

@Composable
private fun SessionRow(
    s: SessionInfo,
    isAttached: Boolean,
    onAttach: () -> Unit,
    onRename: (String) -> Unit,
    onDelete: () -> Unit,
) {
    var renaming by remember { mutableStateOf(false) }
    var deleting by remember { mutableStateOf(false) }
    Row(
        Modifier.fillMaxWidth().clickable { onAttach() }.padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Box(
            Modifier.size(8.dp)
                .background(
                    if (isAttached) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant,
                    CircleShape,
                ),
        )
        Column(Modifier.weight(1f).padding(start = 10.dp)) {
            Text(s.name, fontWeight = if (isAttached) FontWeight.Bold else FontWeight.Normal)
            Text(
                s.dir, style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.outline, maxLines = 1, overflow = TextOverflow.Ellipsis,
            )
        }
        TextButton(onClick = { renaming = true }) { Text("✎") }
        TextButton(onClick = { deleting = true }) { Text("🗑") }
    }
    if (renaming) {
        var newName by remember { mutableStateOf(s.name) }
        AlertDialog(
            onDismissRequest = { renaming = false },
            title = { Text("Rename session") },
            text = {
                OutlinedTextField(newName, { newName = it }, singleLine = true, label = { Text("Name") })
            },
            confirmButton = { TextButton(onClick = { onRename(newName); renaming = false }) { Text("Rename") } },
            dismissButton = { TextButton(onClick = { renaming = false }) { Text("Cancel") } },
        )
    }
    if (deleting) {
        AlertDialog(
            onDismissRequest = { deleting = false },
            title = { Text("Delete session?") },
            text = { Text("Remove \"${s.name}\"? This deletes the session record (the directory is left untouched).") },
            confirmButton = { TextButton(onClick = { onDelete(); deleting = false }) { Text("Delete") } },
            dismissButton = { TextButton(onClick = { deleting = false }) { Text("Cancel") } },
        )
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

@Composable
private fun ServerSettings(settings: SettingsStore, onSaveConnect: (String, String) -> Unit, onBack: () -> Unit) {
    var url by rememberSaveable { mutableStateOf(settings.url) }
    var token by rememberSaveable { mutableStateOf(settings.token) }
    SettingsScaffold("Server", onBack) {
        OutlinedTextField(url, { url = it }, label = { Text("Server URL") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        OutlinedTextField(token, { token = it }, label = { Text("Token") }, singleLine = true, modifier = Modifier.fillMaxWidth())
        Button(onClick = { settings.url = url; settings.token = token; onSaveConnect(url, token) }) {
            Text("Save & Connect")
        }
        Text("Client ID: ${settings.clientId}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
    }
}

@Composable
private fun AppearanceSettings(themeMode: ThemeMode, onThemeChange: (ThemeMode) -> Unit, onBack: () -> Unit) {
    SettingsScaffold("Appearance", onBack) {
        Text("Theme", style = MaterialTheme.typography.titleMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            ThemeChoice("System", themeMode == ThemeMode.SYSTEM) { onThemeChange(ThemeMode.SYSTEM) }
            ThemeChoice("Light", themeMode == ThemeMode.LIGHT) { onThemeChange(ThemeMode.LIGHT) }
            ThemeChoice("Dark", themeMode == ThemeMode.DARK) { onThemeChange(ThemeMode.DARK) }
        }
    }
}

private data class CommandInfo(val name: String, val desc: String)

private val COMMANDS = listOf(
    CommandInfo("attach", "Attach to a session by name"),
    CommandInfo("detach", "Leave the current session"),
    CommandInfo("list", "List your sessions"),
    CommandInfo("status", "What the session is doing"),
    CommandInfo("kill", "Delete a session by name"),
    CommandInfo("spawn", "Start a new session or project"),
    CommandInfo("cancel", "Discard the current message"),
    CommandInfo("help", "Speak the list of commands"),
)

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
private fun CommandAliasGroup(cmd: CommandInfo, aliases: List<String>, onAdd: (String) -> Unit, onRemove: (String) -> Unit) {
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
                    Text(cmd.desc, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
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
    var sttMode by remember { mutableStateOf(settings.sttMode) }
    var sttModel by remember { mutableStateOf(settings.sttModel) }
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
        Text("Transcription (Whisper)", style = MaterialTheme.typography.titleMedium)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            ThemeChoice("Dynamic", sttMode == "dynamic") { sttMode = "dynamic"; settings.sttMode = "dynamic"; onSttChanged() }
            ThemeChoice("Fixed", sttMode == "fixed") { sttMode = "fixed"; settings.sttMode = "fixed"; onSttChanged() }
        }
        if (sttMode == "fixed") {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                ThemeChoice("Fast", sttModel == "tiny") { sttModel = "tiny"; settings.sttModel = "tiny"; onSttChanged() }
                ThemeChoice("Balanced", sttModel == "base") { sttModel = "base"; settings.sttModel = "base"; onSttChanged() }
                ThemeChoice("Accurate", sttModel == "small") { sttModel = "small"; settings.sttModel = "small"; onSttChanged() }
            }
        }
        Text(
            if (sttMode == "dynamic") "Short clips use the fast model, longer clips the accurate one."
            else "Every clip uses the chosen model (Accurate = best, slowest).",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

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
private fun BrowseScreen(controller: VoiceController, onStarted: () -> Unit, onBack: () -> Unit) {
    val listing by controller.listing.collectAsStateWithLifecycle()
    LaunchedEffect(Unit) { controller.browse("") } // load the roots on open
    val atRoots = listing?.path.isNullOrEmpty()

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
        Button(
            onClick = { listing?.path?.let { controller.spawnAt(it) }; onStarted() },
            enabled = !atRoots,
            modifier = Modifier.fillMaxWidth().padding(top = 8.dp),
        ) { Text(if (atRoots) "Choose a folder…" else "Start session here") }
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

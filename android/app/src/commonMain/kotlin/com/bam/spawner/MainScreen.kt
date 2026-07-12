package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectHorizontalDragGestures
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.FilterChip
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.PermanentDrawerSheet
import androidx.compose.material3.PermanentNavigationDrawer
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.unit.dp
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.DiscoveredInfo
import kotlinx.coroutines.flow.drop
import kotlinx.coroutines.flow.first
import kotlinx.coroutines.launch
import kotlinx.coroutines.withTimeoutOrNull

/**
 * The main chat screen: the navigation drawer (sessions list), top bar, chat log, the status
 * bars, and the input bar, plus the hoisted session-list and usage dialogs. Reads the shared
 * app state off [controller]; the audio-hardware surface (mic status text, output picker,
 * push-to-talk) is passed in as values + callbacks so this stays free of the concrete class.
 * The 📎 transfer button is a [transferButton] slot (Android SAF; web empty until M5).
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun MainScreen(
    controller: AppController,
    handsFreeInitial: Boolean,
    badgeMode: String,
    showCacheTimer: Boolean,
    trayCommandNames: Set<String>,
    debugOverlays: Boolean = false,
    mic: String,
    audioOutput: AudioOutput,
    audioOutputs: List<AudioOutput>,
    audioInput: AudioInput,
    audioInputs: List<AudioInput>,
    onToggleHandsFree: (Boolean) -> Unit,
    onSelectAudioOutput: (AudioOutput) -> Unit,
    onSelectAudioInput: (AudioInput) -> Unit,
    onRefreshOutputs: () -> Unit,
    onTalkStart: () -> Unit,
    onTalkStop: () -> Unit,
    onTalkCancel: () -> Unit,
    onStopSpeaking: () -> Unit,
    onOpenSettings: () -> Unit,
    onNewSession: () -> Unit,
    transferButton: @Composable (onUploaded: (String) -> Unit) -> Unit = { },
) {
    val drawerState = rememberDrawerState(DrawerValue.Closed)
    val scope = rememberCoroutineScope()
    val focus = LocalFocusManager.current
    // System back closes the open drawer instead of leaving the app.
    PlatformBackHandler(enabled = drawerState.isOpen) { scope.launch { drawerState.close() } }
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

    val status by controller.status.collectAsState()
    val connected by controller.connected.collectAsState()
    val chat by controller.chat.collectAsState()
    val hasMoreHistory by controller.hasMoreHistory.collectAsState()
    val scrollTick by controller.scrollTick.collectAsState()
    val discovered by controller.discovered.collectAsState()
    val discoverError by controller.discoverError.collectAsState()
    val attached by controller.attachedName.collectAsState()
    val attachedId by controller.attachedId.collectAsState()
    val attachedAgent by controller.attachedAgent.collectAsState()
    val attachedModel by controller.attachedModel.collectAsState()
    val agents by controller.agents.collectAsState()
    // Hoisted dialogs for the drawer's session list. A card expands in place to its
    // Open/Edit/Delete actions, which fan out to these.
    var confirmOpen by remember { mutableStateOf<DiscoveredInfo?>(null) }
    var deleteTarget by remember { mutableStateOf<DiscoveredInfo?>(null) }
    var editTarget by remember { mutableStateOf<DiscoveredInfo?>(null) }
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
    // Per-session "activity we've already surfaced", keyed by stable session id. Seeded to a
    // session's current lastActive the first time we see it (so nothing is falsely unread on
    // first load) and kept current for the session you're attached to. A session only becomes
    // "unread" — and thus orange in the sidebar — when new output lands for it while you're
    // attached elsewhere. In-memory: a fresh launch starts everyone clean.
    val seen = remember { mutableStateMapOf<String, Long>() }
    LaunchedEffect(discovered, attachedId) {
        discovered.forEach { d ->
            val id = d.sessionId.ifBlank { d.dir }
            val prev = seen[id]
            if (prev == null || id == attachedId) seen[id] = maxOf(prev ?: 0L, d.lastActive)
        }
    }
    val unread = discovered.mapNotNull { d ->
        val id = d.sessionId.ifBlank { d.dir }
        val mark = seen[id]
        if (d.sessionId != attachedId && mark != null && d.lastActive > mark) id else null
    }.toSet()
    val openSession = { d: DiscoveredInfo ->
        controller.adopt(d.sessionId, d.dir); scope.launch { drawerState.close() }; Unit
    }
    val voiceState by controller.voiceState.collectAsState()
    val ask by controller.ask.collectAsState()
    val speaking by controller.speaking.collectAsState()
    val pending by controller.pending.collectAsState()
    val activity by controller.activity.collectAsState()
    val lastUsage by controller.lastTurnUsage.collectAsState()
    val rateLimit by controller.rateLimit.collectAsState()
    val usageReport by controller.usageReport.collectAsState()
    val usageLoading by controller.usageLoading.collectAsState()
    val usageEstimate by controller.usageEstimate.collectAsState()
    var handsFree by remember { mutableStateOf(handsFreeInitial) }
    // The command tray (swipe up on the message box). Hoisted here so a tap
    // anywhere outside it — the chat, the bars, the text field — can dismiss it.
    var trayOpen by rememberSaveable { mutableStateOf(false) }

    // The sessions sidebar. Reused verbatim by both layouts — [onNavigated] closes the
    // drawer in the narrow layout (a no-op in the wide one, which pins it open).
    val sidebar: @Composable (onNavigated: () -> Unit) -> Unit = { onNavigated ->
        Sidebar(
            discovered = discovered,
            discoverError = discoverError,
            agents = agents,
            attached = attached,
            attachedId = attachedId,
            unread = unread,
            onNew = { onNewSession(); onNavigated() },
            refreshing = refreshing,
            onRefresh = { refreshing = true },
            onOpen = { d -> if (d.active) confirmOpen = d else { openSession(d); onNavigated() } },
            onEdit = { editTarget = it },
            onDelete = { deleteTarget = it },
            onDetach = { controller.detach() },
            rateLimit = rateLimit,
            usageEstimate = usageEstimate,
            onCheckUsage = { controller.requestUsage(); onNavigated() },
        )
    }
    // The chat column (top bar → list → status bars → input bar). [onMenu] is null in
    // the wide layout, which drops the ☰ toggle since the sidebar is always visible.
    val chatColumn: @Composable (onMenu: (() -> Unit)?) -> Unit = { onMenu ->
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
                modelBadge = if (attached != null) backendBadge(agents, attachedAgent, attachedModel) else "",
                contextTokens = lastUsage?.usage?.contextTokens,
                onMenu = onMenu,
                onSettings = onOpenSettings,
                audioOutput = audioOutput,
                audioOutputs = audioOutputs,
                onSelectOutput = onSelectAudioOutput,
                audioInput = audioInput,
                audioInputs = audioInputs,
                onSelectInput = onSelectAudioInput,
                onOutputMenuOpened = onRefreshOutputs,
            )
            if (attached == null) DetachedBanner()
            // The status bars below the list are Column siblings: showing one shrinks
            // the list from the bottom. ChatList watches its own viewport height and
            // re-pins the newest message above the bars (and the keyboard) itself.
            val showWarmBar = showCacheTimer && lastUsage != null
            ChatList(chat, hasMoreHistory, scrollTick, badgeMode, controller::loadOlder, Modifier.weight(1f).fillMaxWidth())
            if (showWarmBar) lastUsage?.let { CacheWarmBar(it) }
            if (speaking) SpeakingBar(onStop = onStopSpeaking)
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
                trayCommandNames = trayCommandNames,
                // While hands-free owns the mic, push-to-talk is disabled — but the
                // button still accepts a swipe-up to toggle hands-free back off.
                handsFree = handsFree,
                onToggleHandsFree = { on -> handsFree = on; onToggleHandsFree(on) },
                onTalkStart = onTalkStart,
                onTalkStop = onTalkStop,
                onTalkCancel = onTalkCancel,
                onSend = { controller.sendText(it) },
                transferButton = transferButton,
                debugOverlays = debugOverlays,
            )
        }
    }

    // Responsive layout: on a wide window (desktop browser, tablet, unfolded) the
    // sidebar is pinned permanently beside the chat; on a narrow one (phone) it lives
    // in a swipe-in modal drawer. Same composables, different container. 840.dp is the
    // Material "expanded" width breakpoint.
    BoxWithConstraints(Modifier.fillMaxSize()) {
      if (maxWidth >= 840.dp) {
        PermanentNavigationDrawer(
            drawerContent = {
                PermanentDrawerSheet(Modifier.width(320.dp)) { sidebar {} }
            },
        ) {
            chatColumn(null)
        }
      } else {
        ModalNavigationDrawer(
            drawerState = drawerState,
            // Opened by the ☰ button or a left-edge swipe (a narrow strip on the far
            // left, see below). We keep the drawer's own gestures limited to when it's
            // already open (swipe-to-close) rather than enabling them for the whole
            // content, which would let any horizontal drag across the chat open it.
            gesturesEnabled = drawerState.isOpen,
            drawerContent = {
                ModalDrawerSheet { sidebar { scope.launch { drawerState.close() } } }
            },
        ) {
          Box(Modifier.fillMaxSize()) {
            chatColumn { scope.launch { drawerState.open() } }
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
    // Edit: rename plus (when more than one backend is advertised) switch the
    // session's AI agent + model. Changing the backend restarts the conversation on
    // the new AI — the dialog warns before you commit.
    editTarget?.let { d ->
        var newName by remember(d) { mutableStateOf(d.name) }
        val curAgent = d.agent.ifBlank { "claude" } // "" on the wire == the default Claude backend
        var selAgent by remember(d) { mutableStateOf(curAgent) }
        val agentInfo = agents.firstOrNull { it.id == selAgent }
        var selModel by remember(d) { mutableStateOf(d.model) }
        // Keep the model valid for the chosen backend; snap to its default otherwise.
        LaunchedEffect(selAgent, agents) {
            agentInfo?.let { if (it.models.none { m -> m == selModel }) selModel = it.defaultModel }
        }
        AlertDialog(
            onDismissRequest = { editTarget = null },
            title = { Text("Edit session") },
            text = {
                Column {
                    OutlinedTextField(newName, { newName = it }, singleLine = true, label = { Text("Name") })
                    if (agents.size > 1) {
                        Spacer(Modifier.height(10.dp))
                        Text("AI agent", style = MaterialTheme.typography.labelMedium)
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                            agents.forEach { a ->
                                FilterChip(selected = selAgent == a.id, onClick = { selAgent = a.id },
                                    label = { Text(a.name) })
                            }
                        }
                    }
                    agentInfo?.takeIf { it.models.isNotEmpty() }?.let { a ->
                        Spacer(Modifier.height(8.dp))
                        Text("Model", style = MaterialTheme.typography.labelMedium)
                        FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                            a.models.forEach { m ->
                                FilterChip(selected = selModel == m, onClick = { selModel = m },
                                    label = { Text(m) })
                            }
                        }
                    }
                    if (selAgent != curAgent) {
                        Spacer(Modifier.height(8.dp))
                        Text("Switching agent starts a fresh conversation on ${agentInfo?.name ?: selAgent} — " +
                            "the old history stays on disk but won't carry over.",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.error)
                    }
                }
            },
            confirmButton = {
                TextButton(onClick = {
                    if (newName.isNotBlank() && newName != d.name)
                        controller.renameDiscovered(d.sessionId, d.dir, newName)
                    if (selAgent != curAgent || selModel != d.model)
                        controller.setAgent(d.sessionId, d.dir, selAgent, selModel)
                    editTarget = null
                }) { Text("Save") }
            },
            dismissButton = { TextButton(onClick = { editTarget = null }) { Text("Cancel") } },
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

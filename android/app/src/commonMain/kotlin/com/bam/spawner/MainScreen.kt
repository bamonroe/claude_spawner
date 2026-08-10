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
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.imePadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.systemBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.FilterChip
import androidx.compose.material3.DrawerValue
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.ModalDrawerSheet
import androidx.compose.material3.ModalNavigationDrawer
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.PermanentDrawerSheet
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.rememberDrawerState
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.unit.dp
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.ServerMsg
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
    /** The configured radial-menu tree — see `RadialMenuConfig.kt`. */
    radialMenu: RadialMenuConfig = DefaultRadialMenu,
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
    /** Persisted: keep the sidebar docked open beside the chat (see [Prefs.sidebarPinned]). */
    sidebarPinned: Boolean = false,
    onSidebarPinnedChange: (Boolean) -> Unit = { },
    transferButton: @Composable (onUploaded: (String) -> Unit) -> Unit = { },
    /** Open the Claude login screen for a host — the inline offer after a turn dies on
     *  expired credentials. No-op by default so hosts without the route opt out. */
    onClaudeLogin: (String) -> Unit = { },
) {
    var pinned by remember { mutableStateOf(sidebarPinned) }
    val setPinned: (Boolean) -> Unit = { pinned = it; onSidebarPinnedChange(it) }
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
    val restartBuilding by controller.restartBuilding.collectAsState()
    val restartPending by controller.restartPending.collectAsState()
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
    // Which sessions are holding output you haven't seen — see [rememberUnreadSessions]; it and
    // [needsAttention] are the one definition of the orange cue every session surface reads.
    val unread = rememberUnreadSessions(discovered, attachedId)
    val openSession = { d: DiscoveredInfo ->
        if (d.registered) controller.focusSession(d) else controller.adopt(d.sessionId, d.dir)
        // Opening a session answers any heads-up we were showing for it.
        controller.dismissNotice(d.sessionId)
        scope.launch { drawerState.close() }; Unit
    }
    // Heads-ups about other sessions (finished background jobs). Tapping one opens that
    // session when we can still see it in the discovered list; otherwise it just dismisses.
    val notices by controller.notices.collectAsState()
    val authStates by controller.authStates.collectAsState()
    val openNotice = { n: ServerMsg.Notice ->
        val d = discovered.firstOrNull { it.sessionId == n.sessionId }
        if (d != null) openSession(d) else controller.dismissNotice(n.sessionId)
    }
    val voiceState by controller.voiceState.collectAsState()
    val speechGate by controller.speechGate.collectAsState()
    val ask by controller.ask.collectAsState()
    val speaking by controller.speaking.collectAsState()
    val pending by controller.pending.collectAsState()
    val activity by controller.activity.collectAsState()
    val lastUsage by controller.lastTurnUsage.collectAsState()
    val rateLimit by controller.rateLimit.collectAsState()
    val usageReport by controller.usageReport.collectAsState()
    val usageLoading by controller.usageLoading.collectAsState()
    var handsFree by remember { mutableStateOf(handsFreeInitial) }
    // The command tray (swipe up on the message box). Hoisted here so a tap
    // anywhere outside it — the chat, the bars, the text field — can dismiss it.
    var trayOpen by rememberSaveable { mutableStateOf(false) }
    // The radial session palette, opened by double-tapping the transcript. Hoisted so
    // both layouts share one state; the ring itself renders over the whole screen.
    var paletteOpen by remember { mutableStateOf(false) }
    // Where the double-tap landed, in root coordinates — the ring blooms from there.
    var paletteAt by remember { mutableStateOf<Offset?>(null) }
    // Mic lock: the palette's centre button holds the ordinary tap-to-speak capture
    // open, so you can talk without keeping a finger down. Same path as a hold — it
    // just doesn't end until you release the lock. Not hands-free: no wake word, no
    // VAD, one clip that ends when the lock does.
    var micLocked by remember { mutableStateOf(false) }
    // Release the lock and send what's been captured so far. Safe to call when unlocked.
    val releaseMicLock: () -> Unit = { if (micLocked) { micLocked = false; onTalkStop() } }
    // Drop the lock and throw the clip away (used when the capture can no longer be
    // delivered as recorded: the socket dropped, or hands-free is taking the mic).
    val abandonMicLock: () -> Unit = { if (micLocked) { micLocked = false; onTalkCancel() } }
    // The lock outlives the palette (closing the ring leaves it recording) but never
    // outlives the screen being visible: leaving the app ends the clip and sends it.
    PlatformBackgroundEffect { releaseMicLock() }
    // Losing the socket makes the clip undeliverable — drop it rather than leave a
    // dead lock on screen. Switching sessions ends the clip too, so the capture lands
    // in the session it was started in (the slot path stops before it attaches; this
    // catches every other route to a new focus — sidebar, notice, voice command).
    LaunchedEffect(connected) { if (!connected) abandonMicLock() }
    LaunchedEffect(attachedId) { releaseMicLock() }

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
            onCheckUsage = { controller.requestUsage(); onNavigated() },
            pinned = pinned,
            // Pinning from the overlay docks it (and closes the overlay behind it);
            // unpinning from the docked rail hides it back behind the ☰ button.
            onTogglePinned = { on -> setPinned(on); if (on) onNavigated() },
            connected = connected,
            restartBuilding = restartBuilding,
            restartPending = restartPending,
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
            // A turn that died on expired credentials: nothing on that host will run
            // until it's logged in again, so offer the flow here rather than making the
            // user find it in Hosts settings.
            authStates.filterValues { it.needsLogin && !it.loggedIn }.keys.forEach { h ->
                ClaudeLoginNeededBanner(h, onLogin = { onClaudeLogin(h) })
            }
            notices.forEach { n ->
                NoticeBanner(n, onOpen = { openNotice(n) }, onDismiss = { controller.dismissNotice(n.sessionId) })
            }
            // The status bars below the list are Column siblings: showing one shrinks
            // the list from the bottom. ChatList watches its own viewport height and
            // re-pins the newest message above the bars (and the keyboard) itself.
            val showWarmBar = showCacheTimer && lastUsage != null
            ChatList(
                chat, hasMoreHistory, scrollTick, badgeMode, attachedId, controller::loadOlder,
                Modifier.weight(1f).fillMaxWidth(),
                onDoubleTap = { at -> trayOpen = false; paletteAt = at; paletteOpen = true },
            )
            if (showWarmBar) lastUsage?.let { CacheWarmBar(it) }
            if (speaking) SpeakingBar(onStop = onStopSpeaking)
            if (activity.isNotBlank()) ActivityIndicator(activity, onAbort = controller::abortTurn)
            if (pending.isNotBlank()) DraftLine(pending)
            if (handsFree) VoiceStatePill(voiceState)
            SpeechGatePill(speechGate)
            if (micLocked) {
                Text(
                    "🔴 mic locked — tap the mic to send",
                    color = MaterialTheme.colorScheme.error,
                    modifier = Modifier.padding(horizontal = 12.dp, vertical = 2.dp),
                    style = MaterialTheme.typography.labelMedium,
                )
            }
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
                // Hands-free takes the mic, so it can't coexist with a held-open clip.
                onToggleHandsFree = { on -> if (on) abandonMicLock(); handsFree = on; onToggleHandsFree(on) },
                micLocked = micLocked,
                onReleaseMicLock = releaseMicLock,
                onTalkStart = onTalkStart,
                onTalkStop = onTalkStop,
                onTalkCancel = onTalkCancel,
                onSend = { controller.sendText(it) },
                transferButton = transferButton,
                debugOverlays = debugOverlays,
            )
        }
    }

    // Unpinned (the default), the sidebar is a modal drawer at every width, opened by the
    // ☰ button in the top bar or by a swipe from the far left edge, so the chat keeps the
    // full window until the user asks for the session list. Pinned
    // (the sidebar's pin toggle, persisted), it becomes a permanent docked rail and the
    // chat column shifts over beside it so no message text is ever covered.
    BoxWithConstraints(Modifier.fillMaxSize()) {
      val containerWidth = maxWidth
      // The chat area plus its edge-swipe strips, laid out identically in both modes.
      val chatArea: @Composable (onMenu: (() -> Unit)?) -> Unit = { onMenu ->
          Box(Modifier.fillMaxSize()) {
            chatColumn(onMenu)
            // Edge-swipe strips stay in the chat area. They are transparent boxes
            // layered over content, so covering the app bar or message bar would steal
            // taps from real controls before those controls ever see the pointer.
            val edgeGestureTopInset = 88.dp
            val edgeGestureBottomInset = 144.dp
            val swapStripWidth = if (containerWidth >= 600.dp) 72.dp else 28.dp
            val swapDragThreshold = if (containerWidth >= 600.dp) 48.dp else 64.dp
            // Right-edge swipe (right→left) to "swap" back to the previously attached
            // session — the gesture twin of the voice "swap" command. A thin strip on the
            // far right that captures a leftward drag; a generous threshold avoids accidental
            // triggers, and a bottom inset keeps it clear of the mic / input bar below.
            Box(
                Modifier.align(Alignment.CenterEnd)
                    .fillMaxHeight()
                    .imePadding()
                    .padding(top = edgeGestureTopInset, bottom = edgeGestureBottomInset)
                    .width(swapStripWidth)
                    .pointerInput(Unit) {
                        val threshold = swapDragThreshold.toPx()
                        var dx = 0f
                        detectHorizontalDragGestures(
                            onDragStart = { dx = 0f },
                            onHorizontalDrag = { _, delta -> dx += delta },
                            onDragEnd = { if (dx <= -threshold) controller.swap() },
                        )
                    },
            )
            // Left-edge swipe (left→right) to open the sidebar — the gesture twin of the
            // ☰ button. Only a thin strip at the very edge is live, so an ordinary
            // horizontal drag across the chat still can't pull the drawer out. In pinned
            // mode onMenu is null and the strip does nothing.
            if (onMenu != null) {
                Box(
                    Modifier.align(Alignment.CenterStart)
                        .fillMaxHeight()
                        .imePadding()
                        .padding(top = edgeGestureTopInset, bottom = edgeGestureBottomInset)
                        .width(swapStripWidth)
                        .pointerInput(Unit) {
                            val threshold = swapDragThreshold.toPx()
                            var dx = 0f
                            detectHorizontalDragGestures(
                                onDragStart = { dx = 0f },
                                onHorizontalDrag = { _, delta -> dx += delta },
                                onDragEnd = { if (dx >= threshold) onMenu() },
                            )
                        },
                )
            }
          }
      }
      if (pinned) {
          Row(Modifier.fillMaxSize()) {
              // The docked rail. Same composable as the drawer sheet, so it keeps the
              // surface treatment; navigating in it doesn't dismiss anything.
              PermanentDrawerSheet(Modifier.width(300.dp)) { sidebar { } }
              // No ☰ in pinned mode — the sidebar is already on screen.
              Box(Modifier.weight(1f).fillMaxHeight()) { chatArea(null) }
          }
      } else {
          ModalNavigationDrawer(
              drawerState = drawerState,
              // Opened only by the ☰ button. The drawer's own gestures stay limited to
              // when it's already open (swipe-to-close); enabling them for the whole
              // content would let any horizontal drag across the chat open it.
              gesturesEnabled = drawerState.isOpen,
              drawerContent = {
                  ModalDrawerSheet { sidebar { scope.launch { drawerState.close() } } }
              },
          ) {
              chatArea { scope.launch { drawerState.open() } }
          }
      }
      // The radial menu. While the mic is locked the ring *is* the recording control —
      // send (centre) and cancel (above it), nothing implicit closes it — so the clip can
      // only end by an explicit tap. Otherwise it shows the configured menu tree.
      if (paletteOpen && micLocked) {
          RadialPalette(
              slots = emptyList(),
              centerLabel = "Send",
              centerIcon = Icons.Filled.Mic,
              centerHighlighted = true,
              cancelLabel = "Cancel",
              onCancel = { abandonMicLock(); paletteOpen = false },
              dismissible = false,
              origin = paletteAt,
              onSlot = { },
              onCenter = { releaseMicLock(); paletteOpen = false },
              onDismiss = { },
          )
      } else if (paletteOpen) {
          // The live sources: the attach-history ring, and the curated tray commands
          // (the same argument-free set the swipe-up tray shows), each as one slot.
          val dynamicEntries: (String) -> List<RadialDynamicEntry> = { source ->
              when (source) {
                RadialSources.COMMANDS ->
                  if (!connected) emptyList()
                  else COMMANDS.filter { c ->
                      c.name in trayCommandNames && c.aliases.none { it.contains("<") }
                  }.map { cmd ->
                      RadialDynamicEntry(PaletteSlot(id = "cmd:" + cmd.name, label = cmd.name)) {
                          releaseMicLock()
                          controller.sendText("hey buddy " + cmd.aliases.first())
                          paletteOpen = false
                      }
                  }
                RadialSources.SESSIONS -> controller.paletteSessions().map { d ->
                  RadialDynamicEntry(
                      PaletteSlot(
                          id = sessionKey(d),
                          label = d.name.ifBlank { d.dir.substringAfterLast('/') },
                          highlighted = needsAttention(d, attachedId, unread),
                      ),
                  ) {
                      // End any locked capture first so the clip is sent against the
                      // session it was spoken into, not the one we're switching to.
                      releaseMicLock()
                      openSession(d)
                      paletteOpen = false
                  }
                }
                else -> emptyList()
              }
          }
          RadialMenuHost(
              config = radialMenu,
              origin = paletteAt,
              dynamic = dynamicEntries,
              onDismiss = { paletteOpen = false },
              onAction = { action ->
                  // Locking the mic keeps the ring up so the centre stays under the thumb
                  // as the send button; every other action closes it.
                  when (action) {
                      RadialActions.MIC_LOCK ->
                          if (connected && !handsFree) { micLocked = true; onTalkStart() }
                          else paletteOpen = false
                      RadialActions.NEW_SESSION -> { onNewSession(); paletteOpen = false }
                      RadialActions.SWAP -> { releaseMicLock(); controller.swap(); paletteOpen = false }
                      RadialActions.DETACH -> { releaseMicLock(); controller.detach(); paletteOpen = false }
                      RadialActions.HANDS_FREE -> {
                          abandonMicLock()
                          handsFree = !handsFree; onToggleHandsFree(handsFree); paletteOpen = false
                      }
                      RadialActions.STOP_SPEAKING -> { onStopSpeaking(); paletteOpen = false }
                      RadialActions.USAGE -> { controller.requestUsage(); paletteOpen = false }
                      RadialActions.SETTINGS -> { releaseMicLock(); onOpenSettings(); paletteOpen = false }
                      else -> paletteOpen = false
                  }
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
    // Edit: rename plus (when more than one backend is advertised) switch the
    // session's AI agent + model. Changing the backend restarts the conversation on
    // the new AI — the dialog warns before you commit.
    editTarget?.let { d ->
        var newName by remember(d) { mutableStateOf(d.name) }
        val curAgent = d.agent.ifBlank { defaultAgentId(agents) } // "" on the wire == the server's default backend
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
                            "a recap of the current one is carried over, and the old messages stay in the log.",
                            style = MaterialTheme.typography.labelSmall)
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
            usageLoading, usageReport,
            onDismiss = { controller.dismissUsage() },
        )
    }
}

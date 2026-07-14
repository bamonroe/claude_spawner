package com.bam.spawner

import androidx.compose.material3.Surface
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.foundation.layout.fillMaxSize
import com.bam.spawner.audio.AudioInput
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.ui.SpawnerTheme
import com.bam.spawner.ui.parseThemeMode

/**
 * The browser client's root: a minimal navigation shell over the shared screens, backed by a
 * [WebAppController] (real WebSocket) and [WebPrefs] (localStorage). Auto-connects on load using
 * the saved URL/token. Browser audio is live (Web Audio push-to-talk + hands-free, SpeechSynthesis
 * TTS); output routing is Speaker/Mute only, since browsers speak to the OS default sink. `AppRoot`
 * stays Android-only, so this is the web equivalent.
 */
@Composable
fun WebRoot() {
    val prefs = remember { WebPrefs() }
    val controller = remember { WebAppController(prefs) }
    var themeMode by remember { mutableStateOf(parseThemeMode(prefs.themeMode)) }
    var screen by remember { mutableStateOf("main") }
    val connected by controller.connected.collectAsState()
    val mic by controller.micText.collectAsState()
    val audioOutput by controller.audioOutput.collectAsState()

    // Connect once on load using the saved server URL + token (edit them under Settings → Server).
    LaunchedEffect(Unit) { controller.connect(prefs.url, prefs.token) }

    val reconnect = { controller.connect(prefs.url, prefs.token) }
    SpawnerTheme(themeMode) {
        Surface(Modifier.fillMaxSize()) {
            when (screen) {
                "settings" -> SettingsHub(onOpen = { screen = it }, onBack = { screen = "main" })
                "set_server" -> ServerSettings(
                    prefs, controller,
                    onSaveConnect = { url, token -> prefs.url = url; prefs.token = token; reconnect(); screen = "settings" },
                    onSttChanged = reconnect,
                    onBack = { screen = "settings" },
                )
                "set_hosts" -> HostsSettings(controller, onBack = { screen = "settings" })
                "set_identities" -> IdentitiesSettings(controller, onBack = { screen = "settings" })
                "set_profiles" -> ProfilesSettings(controller, onBack = { screen = "settings" })
                "set_providers" -> ProvidersSettings(controller, onBack = { screen = "settings" })
                "set_appearance" -> AppearanceSettings(
                    prefs, themeMode,
                    onThemeChange = { themeMode = it; prefs.themeMode = it.name.lowercase() },
                    onBack = { screen = "settings" },
                )
                "set_commands" -> CommandsSettings(prefs, onAliasesChanged = reconnect, onSttChanged = reconnect, onBack = { screen = "settings" })
                "browse" -> BrowseScreen(
                    controller,
                    onStarted = { screen = "main" },
                    onBack = { screen = "main" },
                )
                "set_audio" -> AudioSettings(
                    prefs,
                    controller,
                    onVadChanged = {},
                    onSttChanged = reconnect,
                    onBack = { screen = "settings" },
                )
                else -> MainScreen(
                    controller,
                    // Browser hands-free is a per-session toggle, not auto-started on load:
                    // getUserMedia needs a user gesture, so entering it prompts for the mic.
                    handsFreeInitial = false,
                    badgeMode = prefs.tokenBadge,
                    showCacheTimer = prefs.cacheWarmTimer,
                    trayCommandNames = prefs.trayCommandNames().toSet(),
                    // Push-to-talk, SpeechSynthesis TTS, and VAD-gated hands-free are all live
                    // (M5). Browsers speak to the OS default sink and expose no routing, so the
                    // output control is Speaker (voice on) vs Mute (voice off).
                    mic = mic,
                    audioOutput = audioOutput,
                    audioOutputs = listOf(AudioOutput.SPEAKER, AudioOutput.MUTE),
                    // No mic routing in the browser: an empty input list hides the picker's
                    // input section, leaving a plain speaker/mute output menu.
                    audioInput = AudioInput.DEVICE,
                    audioInputs = emptyList(),
                    onToggleHandsFree = { on -> if (on) controller.startHandsFree() else controller.stopHandsFree() },
                    onSelectAudioOutput = controller::setAudioOutput,
                    onSelectAudioInput = {},
                    onRefreshOutputs = {},
                    onTalkStart = controller::startTalking,
                    onTalkStop = controller::stopTalking,
                    onTalkCancel = controller::cancelTalking,
                    onStopSpeaking = controller::stopSpeaking,
                    onOpenSettings = { screen = "settings" },
                    onNewSession = { screen = "browse" }, // shared BrowseScreen: pick backend/model/host + dir
                    // 📎 upload/download to the session's host, over the same socket.
                    transferButton = { onUploaded ->
                        WebTransferButton(controller, enabled = connected, onUploaded = onUploaded)
                    },
                )
            }
        }
    }
}

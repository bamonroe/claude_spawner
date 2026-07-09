package com.bam.spawner

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.foundation.layout.fillMaxSize
import com.bam.spawner.audio.AudioOutput
import com.bam.spawner.ui.ThemeMode
import com.bam.spawner.ui.parseThemeMode

/**
 * The browser client's root: a minimal navigation shell over the shared screens, backed by a
 * [WebAppController] (real WebSocket) and [WebPrefs] (localStorage). Auto-connects on load using
 * the saved URL/token; the audio hardware the shared [MainScreen] expects is stubbed (no mic /
 * TTS / output routing in the browser until M5). `AppRoot` stays Android-only, so this is the
 * web equivalent.
 */
@Composable
fun WebRoot() {
    val prefs = remember { WebPrefs() }
    val controller = remember { WebAppController(prefs) }
    var themeMode by remember { mutableStateOf(parseThemeMode(prefs.themeMode)) }
    var screen by remember { mutableStateOf("main") }

    // Connect once on load using the saved server URL + token (edit them under Settings → Server).
    LaunchedEffect(Unit) { controller.connect(prefs.url, prefs.token) }

    val reconnect = { controller.connect(prefs.url, prefs.token) }
    val dark = when (themeMode) {
        ThemeMode.SYSTEM -> isSystemInDarkTheme()
        ThemeMode.LIGHT -> false
        ThemeMode.DARK -> true
    }
    MaterialTheme(colorScheme = if (dark) darkColorScheme() else lightColorScheme()) {
        Surface(Modifier.fillMaxSize()) {
            when (screen) {
                "settings" -> SettingsHub(onOpen = { screen = it }, onBack = { screen = "main" })
                "set_server" -> ServerSettings(
                    prefs, controller,
                    onSaveConnect = { url, token -> prefs.url = url; prefs.token = token; reconnect(); screen = "settings" },
                    onBack = { screen = "settings" },
                )
                "set_hosts" -> HostsSettings(controller, onBack = { screen = "settings" })
                "set_identities" -> IdentitiesSettings(controller, onBack = { screen = "settings" })
                "set_appearance" -> AppearanceSettings(
                    prefs, themeMode,
                    onThemeChange = { themeMode = it; prefs.themeMode = it.name.lowercase() },
                    onBack = { screen = "settings" },
                )
                "set_commands" -> CommandsSettings(prefs, onAliasesChanged = reconnect, onBack = { screen = "settings" })
                "set_audio" -> AudioSettings(
                    prefs,
                    onVadChanged = {},
                    onSttChanged = reconnect,
                    onBack = { screen = "settings" },
                )
                else -> MainScreen(
                    controller,
                    handsFreeInitial = false,
                    badgeMode = prefs.tokenBadge,
                    showCacheTimer = prefs.cacheWarmTimer,
                    // Audio hardware isn't available in the browser yet (M5): stub it.
                    mic = "",
                    audioOutput = AudioOutput.MUTE,
                    audioOutputs = listOf(AudioOutput.MUTE),
                    onToggleHandsFree = {},
                    onSelectAudioOutput = {},
                    onRefreshOutputs = {},
                    onTalkStart = {},
                    onTalkStop = {},
                    onTalkCancel = {},
                    onStopSpeaking = {},
                    onOpenSettings = { screen = "settings" },
                    onNewSession = {}, // browser spawn UI is future work (M5+)
                )
            }
        }
    }
}

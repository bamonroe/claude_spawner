package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.Button
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.ui.ThemeMode

/**
 * About: the app version and the exact git commit this bundle was built from, so you
 * can tell which build is installed on any given device. Values come from [BuildInfo],
 * generated at build time by the generateBuildInfo Gradle task.
 */
@Composable
fun AboutSettings(onBack: () -> Unit) {
    SettingsScaffold("About", onBack) {
        Text("Claude Spawner", style = MaterialTheme.typography.titleLarge)
        AboutRow("Version", "${BuildInfo.versionName} (build ${BuildInfo.versionCode})")
        AboutRow("Commit", "${BuildInfo.gitCommitShort} · ${BuildInfo.gitBranch}")
        AboutRow("Full commit", BuildInfo.gitCommit)
        AboutRow("Built", BuildInfo.buildTime)
    }
}

/** A labelled read-only value row for the About page. */
@Composable
private fun AboutRow(label: String, value: String) {
    Column(Modifier.fillMaxWidth()) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.outline)
        Text(value, style = MaterialTheme.typography.bodyLarge)
    }
}

/** Debug: developer overlays and verbose gesture logging, all off by default. */
@Composable
fun DebugSettings(settings: Prefs, onBack: () -> Unit) {
    SettingsScaffold("Debug", onBack) {
        var overlays by remember { mutableStateOf(settings.debugOverlays) }
        Row(verticalAlignment = Alignment.CenterVertically) {
            Column(Modifier.weight(1f)) {
                Text("Hit-zone overlays & logging", style = MaterialTheme.typography.titleMedium)
                Text("Draw translucent boxes over the normally-invisible push-to-talk zones — " +
                    "drag left past the red box to discard a clip, drag up past the amber box to " +
                    "switch into hands-free — with a live drift readout while you hold. Also logs " +
                    "each hold's end reason and finger drift to logcat (tag PTT).",
                    style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            Switch(checked = overlays, onCheckedChange = { overlays = it; settings.debugOverlays = it })
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
                Text("Warm-cache countdown", style = MaterialTheme.typography.titleMedium)
                Text("Display-only: count down the ~5-min window where the next turn reuses a warm prompt cache.",
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

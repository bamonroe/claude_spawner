package com.bam.spawner.ui

import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.darkColorScheme
import androidx.compose.material3.lightColorScheme
import androidx.compose.runtime.Composable

/**
 * Tints the platform's status/navigation-bar chrome to match a light or dark theme.
 * Android drives the real window insets controller; web is a no-op (the browser owns its chrome).
 */
@Composable
expect fun ApplySystemBarAppearance(dark: Boolean)

/**
 * Shared Material3 theme: follows the system or is forced light/dark per [mode], applies the
 * platform system-bar chrome, and installs the color scheme. Both clients use this so the theme
 * (and, on Android, the status-bar tint) resolve identically.
 */
@Composable
fun SpawnerTheme(mode: ThemeMode, content: @Composable () -> Unit) {
    val dark = when (mode) {
        ThemeMode.SYSTEM -> isSystemInDarkTheme()
        ThemeMode.LIGHT -> false
        ThemeMode.DARK -> true
    }
    ApplySystemBarAppearance(dark)
    MaterialTheme(colorScheme = if (dark) darkColorScheme() else lightColorScheme(), content = content)
}

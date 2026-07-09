package com.bam.spawner.ui

/** Theme preference, shared by both clients; `SpawnerTheme` resolves it to light/dark. */
enum class ThemeMode { SYSTEM, LIGHT, DARK }

fun parseThemeMode(s: String): ThemeMode = when (s.lowercase()) {
    "light" -> ThemeMode.LIGHT
    "dark" -> ThemeMode.DARK
    else -> ThemeMode.SYSTEM
}

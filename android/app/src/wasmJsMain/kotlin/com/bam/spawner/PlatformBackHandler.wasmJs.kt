package com.bam.spawner

import androidx.compose.runtime.Composable

// No in-app back stack to intercept in the browser; the drawer/back UI provides its own controls.
@Composable
actual fun PlatformBackHandler(enabled: Boolean, onBack: () -> Unit) {
}

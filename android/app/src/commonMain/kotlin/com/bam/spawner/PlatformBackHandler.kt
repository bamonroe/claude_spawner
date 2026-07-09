package com.bam.spawner

import androidx.compose.runtime.Composable

/**
 * Handle the system Back gesture while [enabled]. On Android this is the hardware/gesture
 * back button (used to close the drawer / pop a sub-screen); on the web there is no
 * in-app back stack to intercept, so the actual is a no-op.
 */
@Composable
expect fun PlatformBackHandler(enabled: Boolean, onBack: () -> Unit)

package com.bam.spawner

import androidx.compose.runtime.Composable
import androidx.compose.runtime.rememberUpdatedState
import androidx.lifecycle.compose.LifecycleStartEffect

@Composable
actual fun PlatformBackgroundEffect(onBackground: () -> Unit) {
    val cb = rememberUpdatedState(onBackground)
    LifecycleStartEffect(Unit) { onStopOrDispose { cb.value() } }
}

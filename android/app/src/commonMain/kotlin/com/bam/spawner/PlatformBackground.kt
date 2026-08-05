package com.bam.spawner

import androidx.compose.runtime.Composable

/**
 * Run [onBackground] when the app stops being visible to the user — Android's `onStop`,
 * the browser tab going hidden. Used to release state that must not outlive a visible
 * screen (today: the locked-open microphone, which would otherwise keep recording after
 * the user has walked away from the app).
 */
@Composable
expect fun PlatformBackgroundEffect(onBackground: () -> Unit)

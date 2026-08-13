package com.bam.spawner

import android.app.Activity
import android.content.ContextWrapper
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.ui.platform.LocalContext
import androidx.lifecycle.compose.LifecycleStartEffect

@Composable
actual fun PlatformBackgroundEffect(onBackground: () -> Unit) {
    val cb = rememberUpdatedState(onBackground)
    val context = LocalContext.current
    val activity = remember(context) {
        generateSequence(context) { (it as? ContextWrapper)?.baseContext }
            .filterIsInstance<Activity>().firstOrNull()
    }
    LifecycleStartEffect(Unit) {
        onStopOrDispose {
            // A configuration change (rotating the phone, a theme or font-size change)
            // stops and destroys the Activity only to build it straight back — the app
            // never left the screen, so this isn't backgrounding and must not end a
            // locked-open capture. Rotating used to close the mic and send the clip.
            if (activity?.isChangingConfigurations != true) cb.value()
        }
    }
}

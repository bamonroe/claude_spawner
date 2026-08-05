package com.bam.spawner

import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.rememberUpdatedState
import kotlinx.browser.document
import org.w3c.dom.events.Event

/** The tab is hidden (backgrounded, or the window minimised) — read straight from the DOM. */
private fun pageHidden(): Boolean = js("(document.visibilityState === 'hidden')")

@Composable
actual fun PlatformBackgroundEffect(onBackground: () -> Unit) {
    val cb = rememberUpdatedState(onBackground)
    DisposableEffect(Unit) {
        val listener: (Event) -> Unit = { if (pageHidden()) cb.value() }
        document.addEventListener("visibilitychange", listener)
        onDispose { document.removeEventListener("visibilitychange", listener) }
    }
}

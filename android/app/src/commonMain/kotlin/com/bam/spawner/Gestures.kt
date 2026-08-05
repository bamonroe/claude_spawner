package com.bam.spawner

import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.composed
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.input.pointer.AwaitPointerEventScope
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.onGloballyPositioned
import androidx.compose.ui.layout.positionInRoot

/**
 * Watches for a double-tap **without consuming anything**.
 *
 * The chat viewport already owns every pointer event it cares about: the LazyColumn
 * scrolls, bubbles long-press to select, links and the jump-to-latest button take
 * single taps. A normal `detectTapGestures(onDoubleTap = …)` would sit above or below
 * those and consume downs, breaking one of them. So this observes the *Initial* pass —
 * it sees every down/up before the children do, never consumes, and simply gives up
 * the moment the gesture stops looking like a clean two-finger-free double tap
 * (movement past touch slop, a long press, a second pointer, or a timeout).
 *
 * Because it consumes nothing, a double-tap on a bubble or a link still does whatever
 * that child does; the palette just opens as well.
 */
fun Modifier.observeDoubleTap(enabled: Boolean = true, onDoubleTap: (Offset) -> Unit): Modifier =
    composed {
        // The tap point is reported in *root* coordinates so callers can anchor a
        // full-screen overlay (the radial palette) at it without knowing where this
        // element sits in the layout.
        var origin by remember { mutableStateOf(Offset.Zero) }
        Modifier
            .onGloballyPositioned { origin = it.positionInRoot() }
            .pointerInput(enabled, onDoubleTap) {
                if (!enabled) return@pointerInput
                awaitEachGesture {
                    val first = awaitCleanTap() ?: return@awaitEachGesture
                    // Second tap must start within the platform's double-tap window and land
                    // near the first one — otherwise it's two unrelated taps.
                    val second = withTimeoutOrNull(viewConfiguration.doubleTapTimeoutMillis) {
                        awaitCleanTap()
                    } ?: return@awaitEachGesture
                    if ((second - first).getDistance() <= viewConfiguration.touchSlop * 4) {
                        onDoubleTap(second + origin)
                    }
                }
            }
    }

/**
 * Awaits one tap on the Initial pass: a down and a matching up, with no extra pointer,
 * no movement past touch slop, and no long press. Returns where it landed, or null if
 * the gesture was anything else.
 */
private suspend fun AwaitPointerEventScope.awaitCleanTap(): Offset? {
    var down: Offset? = null
    val slop = viewConfiguration.touchSlop
    return withTimeoutOrNull(viewConfiguration.longPressTimeoutMillis) {
        while (true) {
            val event = awaitPointerEvent(PointerEventPass.Initial)
            val changes = event.changes
            if (changes.size > 1) return@withTimeoutOrNull null
            val change = changes.firstOrNull() ?: return@withTimeoutOrNull null
            val start = down
            if (start == null) {
                if (!change.pressed) return@withTimeoutOrNull null
                down = change.position
            } else {
                if ((change.position - start).getDistance() > slop) return@withTimeoutOrNull null
                if (!change.pressed) return@withTimeoutOrNull change.position
            }
        }
        null
    }
}

package com.bam.spawner

import androidx.compose.foundation.systemGestureExclusion
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Rect

// Reserve the button rect grown down into the nav-bar zone and left along the cancel
// track, so the system back/home gestures can't steal an in-progress push-to-talk hold.
// The rect is in the button's local pixel coords; the OS clamps it to the view and to
// its 200dp-per-edge cap. Only reserved while [active] (the button is a live mic), so it
// doesn't tie up the corner's system gestures the rest of the time.
actual fun Modifier.pttGestureExclusion(active: Boolean, leftPx: Int, rightPx: Int, bottomPx: Int): Modifier =
    if (!active) this
    else systemGestureExclusion { coords ->
        val s = coords.size
        Rect(
            left = -leftPx.toFloat(),
            top = 0f,
            right = (s.width + rightPx).toFloat(),
            bottom = (s.height + bottomPx).toFloat(),
        )
    }

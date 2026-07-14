package com.bam.spawner

import androidx.compose.ui.Modifier

/**
 * Reserve the mic button's region (and an expanded margin below/left of it) from the
 * platform's own edge gestures while [active].
 *
 * On Android the push-to-talk hold was being cut short mid-clip: when the thumb drifts
 * toward the right screen edge (the back-swipe zone) or down into the navigation-bar /
 * home zone, the system claims the in-progress touch and delivers our button a CANCEL —
 * the pointer id vanishes, the gesture loop sees `change == null` ("lost-pointer") and
 * commits the truncated clip. `systemGestureExclusion` tells the OS not to interpret its
 * own gestures inside the given rect, so the touch stays with us for the whole hold. The
 * rect is grown down by [bottomPx] (into the nav-bar zone, past the button's inset), left
 * by [leftPx] (along the cancel-drag track) and right by [rightPx] (across the row's inset
 * to the screen edge, where the back-swipe zone lives just past the button) since the
 * button sits in the corner.
 *
 * Only Android has these edge gestures; the web/desktop actual is a no-op.
 */
expect fun Modifier.pttGestureExclusion(active: Boolean, leftPx: Int, rightPx: Int, bottomPx: Int): Modifier

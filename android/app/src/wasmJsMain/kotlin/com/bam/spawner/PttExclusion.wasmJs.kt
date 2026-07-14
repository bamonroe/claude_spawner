package com.bam.spawner

import androidx.compose.ui.Modifier

// The browser has no system edge gestures to fight over; nothing to reserve.
actual fun Modifier.pttGestureExclusion(active: Boolean, leftPx: Int, rightPx: Int, bottomPx: Int): Modifier = this

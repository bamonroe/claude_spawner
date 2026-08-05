package com.bam.spawner

import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.background
import androidx.compose.foundation.focusable
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.BoxWithConstraints
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.draw.scale
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import kotlin.math.PI
import kotlin.math.cos
import kotlin.math.sin

/** How many slots the ring shows at most. Eight is what a thumb can hit without crowding. */
const val PALETTE_SLOTS = 8

private val SlotSize = 76.dp
private val CenterSize = 88.dp
private val RingRadius = 116.dp

/** One entry on the ring. Deliberately UI-shaped so the web client can build it too. */
data class PaletteSlot(
    val id: String,
    val label: String,
    /** Renders in [BuddyOrange] — busy or unread, matching the sidebar's cue. */
    val highlighted: Boolean = false,
)

/**
 * The radial session palette: a centre button ringed by up to [PALETTE_SLOTS] slots.
 *
 * Slot 0 sits directly above the centre (the most recently used session) and the rest
 * continue clockwise into the past; with fewer than [PALETTE_SLOTS] entries they spread
 * evenly around the whole circle rather than leaving a gap. The ring grows out of
 * [origin] — the point that was double-tapped — clamped so it always lands fully
 * on-screen. Tapping the scrim, pressing back, or pressing Escape dismisses it — the
 * browser has no back button to intercept, so Escape is the web client's way out.
 *
 * The centre is a toggle rather than a navigation target; [centerHighlighted] paints it
 * in the error colour to show the toggle is currently on (today: the mic lock).
 *
 * Lives in commonMain so Android and the web client share one implementation.
 */
@Composable
fun RadialPalette(
    slots: List<PaletteSlot>,
    centerLabel: String,
    centerHighlighted: Boolean = false,
    origin: Offset?,
    onSlot: (PaletteSlot) -> Unit,
    onCenter: () -> Unit,
    onDismiss: () -> Unit,
) {
    PlatformBackHandler(enabled = true) { onDismiss() }
    // One animation drives scale and fade, so the ring blooms from the tap point.
    val progress by animateFloatAsState(1f, tween(durationMillis = 140), label = "palette")
    val focus = remember { FocusRequester() }
    LaunchedEffect(Unit) { focus.requestFocus() }
    BoxWithConstraints(
        Modifier.fillMaxSize()
            .background(MaterialTheme.colorScheme.scrim.copy(alpha = 0.55f * progress))
            .pointerInput(Unit) { detectTapGestures { onDismiss() } }
            .focusRequester(focus)
            .focusable()
            .onPreviewKeyEvent { e ->
                if (e.type == KeyEventType.KeyDown && e.key == Key.Escape) {
                    onDismiss()
                    true
                } else false
            },
    ) {
        val density = androidx.compose.ui.platform.LocalDensity.current
        // Keep the whole ring (slot centres plus half a slot) inside the viewport.
        val margin = RingRadius + SlotSize / 2 + 8.dp
        val marginPx = with(density) { margin.toPx() }
        val w = with(density) { maxWidth.toPx() }
        val h = with(density) { maxHeight.toPx() }
        val raw = origin ?: Offset(w / 2f, h / 2f)
        // On a viewport too small to hold the ring there's nothing to clamp to — centre it.
        val cx = if (w < 2 * marginPx) w / 2f else raw.x.coerceIn(marginPx, w - marginPx)
        val cy = if (h < 2 * marginPx) h / 2f else raw.y.coerceIn(marginPx, h - marginPx)
        val center = with(density) { cx.toDp() to cy.toDp() }

        val shown = slots.take(PALETTE_SLOTS)
        val step = if (shown.isEmpty()) 0.0 else 2 * PI / shown.size
        shown.forEachIndexed { i, slot ->
            // -90° puts slot 0 straight up; increasing angle sweeps clockwise on screen.
            val angle = -PI / 2 + i * step
            val r = RingRadius * progress
            RingButton(
                x = center.first + r * cos(angle).toFloat() - SlotSize / 2,
                y = center.second + r * sin(angle).toFloat() - SlotSize / 2,
                size = SlotSize,
                progress = progress,
                container = if (slot.highlighted) BuddyOrange.copy(alpha = 0.9f)
                            else MaterialTheme.colorScheme.secondaryContainer,
                content = if (slot.highlighted) Color.White else MaterialTheme.colorScheme.onSecondaryContainer,
                label = slot.label,
                onClick = { onSlot(slot) },
            )
        }
        RingButton(
            x = center.first - CenterSize / 2,
            y = center.second - CenterSize / 2,
            size = CenterSize,
            progress = progress,
            container = if (centerHighlighted) MaterialTheme.colorScheme.error
                        else MaterialTheme.colorScheme.primaryContainer,
            content = if (centerHighlighted) MaterialTheme.colorScheme.onError
                      else MaterialTheme.colorScheme.onPrimaryContainer,
            label = centerLabel,
            onClick = onCenter,
        )
    }
}

/** One circular, thumb-sized target on the ring, positioned by its top-left corner. */
@Composable
private fun RingButton(
    x: Dp,
    y: Dp,
    size: Dp,
    progress: Float,
    container: Color,
    content: Color,
    label: String,
    onClick: () -> Unit,
) {
    Surface(
        onClick = onClick,
        modifier = Modifier
            .padding(start = x.coerceAtLeast(0.dp), top = y.coerceAtLeast(0.dp))
            .size(size).scale(progress).alpha(progress),
        shape = CircleShape,
        color = container,
        contentColor = content,
        tonalElevation = 6.dp,
    ) {
        Box(Modifier.padding(6.dp), contentAlignment = Alignment.Center) {
            Text(
                label,
                style = MaterialTheme.typography.labelMedium,
                textAlign = TextAlign.Center,
                maxLines = 3,
                overflow = TextOverflow.Ellipsis,
            )
        }
    }
}

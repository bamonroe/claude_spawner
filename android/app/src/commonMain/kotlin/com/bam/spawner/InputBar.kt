package com.bam.spawner

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.awaitEachGesture
import androidx.compose.foundation.gestures.awaitFirstDown
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.detectVerticalDragGestures
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.offset
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.Send
import androidx.compose.material.icons.filled.Headphones
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material.icons.filled.Mic
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.isShiftPressed
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.input.pointer.PointerEventPass
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.unit.dp
import kotlin.math.roundToInt

/**
 * The message composer: an optional swipe-up command tray, the 📎 transfer button
 * (a platform slot — Android's SAF picker, empty on web until M5), the text field
 * with swipe-to-open-tray, and the WhatsApp-style send/mic/headset button. All the
 * gesture logic is pure Compose; audio + hands-free are driven through the callbacks,
 * so the concrete controller never appears here.
 *
 * @param transferButton renders the 📎 button; it is given an `onUploaded(path)` to
 *        prefill the box after a phone→host upload.
 */
@Composable
fun InputBar(
    connected: Boolean,
    trayOpen: Boolean,
    onTrayOpenChange: (Boolean) -> Unit,
    handsFree: Boolean,
    onToggleHandsFree: (Boolean) -> Unit,
    onTalkStart: () -> Unit,
    onTalkStop: () -> Unit,
    onTalkCancel: () -> Unit,
    onSend: (String) -> Unit,
    transferButton: @Composable (onUploaded: (String) -> Unit) -> Unit = { },
    debugOverlays: Boolean = false,
) {
    var draft by rememberSaveable { mutableStateOf("") }
    var talking by remember { mutableStateOf(false) }
    // Debug: live push-to-talk drift, non-null only while a hold is in progress and
    // [debugOverlays] is on. Feeds the on-screen readout above the mic button.
    var pttDrift by remember { mutableStateOf<PttDrift?>(null) }
    // Swipe up on the text box to reveal the argument-free "hey buddy" commands
    // as tappable buttons; a command tap fires it and hides the tray again. The
    // open flag is hoisted so a tap outside the tray can dismiss it (see caller).
    // Non-null while the mic is held: 0f..1f progress of the drag toward the
    // hands-free threshold. Drives the drag track's fill so you can see how far
    // is left. Null hides the track.
    var swipeFraction by remember { mutableStateOf<Float?>(null) }
    val hasText = draft.isNotBlank()
    // While hands-free owns the mic, push-to-talk is disabled.
    val pushToTalkEnabled = !handsFree
    val micLive = connected && pushToTalkEnabled
    // On the browser/desktop client a physical Enter sends (Shift+Enter = newline) —
    // the on-screen mobile clients keep Enter as newline and rely on the send button.
    val enterSends = platformName() == "Web"
    Column(Modifier.fillMaxWidth()) {
      AnimatedVisibility(visible = trayOpen) {
        CommandTray(
            connected = connected,
            onCommand = { phrase -> onSend(phrase); onTrayOpenChange(false) },
        )
      }
      // A visible handle for the swipe-up command tray: makes the gesture discoverable
      // for mouse/desktop users (who can't obviously "swipe up"), and a plain toggle on
      // touch too. The chevron flips to point the way the tray will move.
      Box(
          Modifier.fillMaxWidth().height(18.dp)
              .clickable { onTrayOpenChange(!trayOpen) },
          contentAlignment = Alignment.Center,
      ) {
          Icon(
              if (trayOpen) Icons.Filled.KeyboardArrowDown else Icons.Filled.KeyboardArrowUp,
              contentDescription = if (trayOpen) "Hide commands" else "Show commands",
              modifier = Modifier.size(18.dp),
              tint = MaterialTheme.colorScheme.onSurfaceVariant,
          )
      }
      Row(
        Modifier.fillMaxWidth().padding(8.dp),
        verticalAlignment = Alignment.Bottom,
        horizontalArrangement = Arrangement.spacedBy(8.dp),
    ) {
        // File transfer (📎): upload a phone file to — or download one from — the
        // session's host, prefilling the box with "look at the file at <path>".
        transferButton { path -> draft = "look at the file at $path" }
        OutlinedTextField(
            value = draft, onValueChange = { draft = it },
            placeholder = { Text("Message…") }, singleLine = false, maxLines = 6,
            // Swipe up to open the command tray, swipe down to close it. Taps still
            // fall through to focus the field (a tap never crosses the drag slop).
            // Any touch on the box while the tray is open dismisses it — observed on
            // the Initial pass without consuming, so the tap still positions the
            // cursor and the swipe-open still works (that handler is armed only when
            // the tray is already open). onFocusChanged covers a first-tap focus; this
            // covers a tap when the swipe-to-open already left the box focused.
            modifier = Modifier.weight(1f)
                // Desktop/web: Enter sends, Shift+Enter inserts a newline. Consumed so
                // the newline isn't also typed. No-op on mobile (enterSends is false).
                .onPreviewKeyEvent { e ->
                    if (enterSends && e.type == KeyEventType.KeyDown &&
                        e.key == Key.Enter && !e.isShiftPressed
                    ) {
                        if (connected && draft.isNotBlank()) { onSend(draft); draft = "" }
                        true
                    } else {
                        false
                    }
                }
                .onFocusChanged { if (it.isFocused) onTrayOpenChange(false) }
                .pointerInput(trayOpen) {
                    if (trayOpen) awaitEachGesture {
                        awaitFirstDown(requireUnconsumed = false, pass = PointerEventPass.Initial)
                        onTrayOpenChange(false)
                    }
                }
                .pointerInput(Unit) {
                    val threshold = 32.dp.toPx()
                    var dy = 0f
                    detectVerticalDragGestures(
                        onDragStart = { dy = 0f },
                        onVerticalDrag = { _, delta -> dy += delta },
                        onDragEnd = {
                            if (dy <= -threshold) onTrayOpenChange(true)
                            else if (dy >= threshold) onTrayOpenChange(false)
                        },
                    )
                },
        )
        // One button, WhatsApp-style: SEND when there's text (tap to send, hold to
        // clear); MIC when the box is empty (hold to talk; drag up the track to
        // switch to hands-free); HEADSET when hands-free is on (tap to turn off).
        // The upward drag distance to switch into hands-free — shared so the visual
        // track is exactly as long as the finger must actually travel.
        val swipeUpDp = 120.dp
        val trackWidth = 36.dp // 75% of the 48dp button
        Box(contentAlignment = Alignment.BottomCenter) {
            if (debugOverlays) {
                // Ugly debug zones: where a hold gets reinterpreted. The real anchor is
                // the touch-down point (within ~24dp of the button centre we draw from),
                // so treat these as approximate. Drag past the red box (left) to discard
                // the clip; past the amber box (up) to switch into hands-free. Both are
                // swipeUpDp from the down point.
                Box(
                    Modifier
                        .offset(x = -swipeUpDp, y = (-24).dp)
                        .size(width = swipeUpDp, height = 48.dp)
                        .background(Color(0x33FF0000)),
                )
                Box(
                    Modifier
                        .offset(y = -swipeUpDp - 24.dp)
                        .size(width = 48.dp, height = swipeUpDp)
                        .background(Color(0x33FFC107)),
                )
                // Live drift readout while holding: left/up drift vs the threshold, and
                // how long the hold has lasted — so a spurious mid-hold cut is visible.
                pttDrift?.let { d ->
                    Box(
                        Modifier
                            .offset(y = -swipeUpDp - 64.dp)
                            .clip(RoundedCornerShape(6.dp))
                            .background(Color(0xCC000000))
                            .padding(horizontal = 6.dp, vertical = 3.dp),
                    ) {
                        Text(
                            "L${d.dx.roundToInt()} U${d.dy.roundToInt()} / ${d.threshold.roundToInt()}px  ${d.heldMs}ms",
                            color = Color.White, style = MaterialTheme.typography.labelSmall,
                        )
                    }
                }
            }
            // The drag track: only visible while the mic is held. It shows the
            // path (and how far) you must drag up to switch into hands-free, and
            // fills toward the headset target as you go.
            swipeFraction?.let { frac ->
                Box(
                    Modifier
                        .offset(y = (-54).dp) // float just above the mic button
                        .size(width = trackWidth, height = swipeUpDp)
                        .clip(RoundedCornerShape(trackWidth / 2))
                        .background(MaterialTheme.colorScheme.surfaceVariant),
                    contentAlignment = Alignment.BottomCenter,
                ) {
                    // Fill grows from the bottom up as the drag nears the threshold.
                    Box(
                        Modifier
                            .fillMaxWidth()
                            .fillMaxHeight(frac)
                            .background(MaterialTheme.colorScheme.primary),
                    )
                    // The target at the top of the track.
                    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.TopCenter) {
                        Icon(
                            Icons.Filled.Headphones, contentDescription = "hands-free",
                            modifier = Modifier.padding(top = 3.dp).size(14.dp),
                        )
                    }
                }
            }
            Surface(
                color = when {
                    talking -> MaterialTheme.colorScheme.error
                    handsFree -> MaterialTheme.colorScheme.error // hands-free = live mic; red headset
                    hasText && connected -> MaterialTheme.colorScheme.primary
                    micLive -> MaterialTheme.colorScheme.primary
                    else -> MaterialTheme.colorScheme.surfaceVariant
                },
                shape = CircleShape,
                // Re-arm the gesture whenever the role changes.
                modifier = Modifier.size(48.dp).pointerInput(hasText, handsFree, connected) {
                    // Distance the finger must travel upward for a hold to be
                    // reinterpreted as switching into hands-free instead of push-to-talk.
                    // Deliberately long so a small drift never trips it.
                    val swipeUpPx = swipeUpDp.toPx()
                    when {
                        hasText -> detectTapGestures(
                            onTap = { if (connected) { onSend(draft); draft = "" } },
                            onLongPress = { draft = "" }, // hold clears the box
                        )
                        // Hands-free on: a single tap on the headset turns it off.
                        handsFree -> detectTapGestures(onTap = { onToggleHandsFree(false) })
                        // Empty box + connected + hands-free off: hold to talk, and
                        // drag up past the track to switch into hands-free.
                        connected -> awaitEachGesture {
                            val down = awaitFirstDown(requireUnconsumed = false)
                            down.consume()
                            val startX = down.position.x
                            val startY = down.position.y
                            talking = true; onTalkStart()
                            swipeFraction = 0f // reveal the track
                            var toggled = false
                            var cancelled = false
                            // Why the hold ended — surfaced to logcat when debugOverlays is on.
                            // "lost-pointer" is the suspect for spurious cuts: the OS dropped
                            // our pointer id mid-hold and we can no longer track it.
                            var reason = "up"
                            var dx = 0f; var dy = 0f; var heldMs = 0L
                            while (true) {
                                val event = awaitPointerEvent()
                                val change = event.changes.firstOrNull { it.id == down.id }
                                if (change == null) { reason = "lost-pointer"; break }
                                // Own the gesture: consuming keeps a parent (scroll /
                                // swipe-up tray) from stealing it when the finger drifts
                                // off the small button, so we hold the recording until an
                                // actual finger-lift no matter how far the finger wanders.
                                change.consume()
                                heldMs = change.uptimeMillis - down.uptimeMillis
                                // Leftward / upward drift from the touch-down point (only
                                // these two directions can end the hold early).
                                dx = (startX - change.position.x).coerceAtLeast(0f)
                                dy = (startY - change.position.y).coerceAtLeast(0f)
                                if (debugOverlays) pttDrift = PttDrift(dx, dy, swipeUpPx, heldMs)
                                if (!change.pressed) { reason = "up"; break } // released
                                // Drift left the full track distance = throw the clip away.
                                if (!cancelled && dx >= swipeUpPx) {
                                    cancelled = true; reason = "swipe-left-cancel"
                                    if (talking) { onTalkCancel(); talking = false }
                                    break // discarded; nothing is sent or transcribed
                                }
                                swipeFraction = (dy / swipeUpPx).coerceIn(0f, 1f)
                                if (!toggled && dy >= swipeUpPx) {
                                    toggled = true; reason = "swipe-up-handsfree"
                                    // Abandon the in-progress push-to-talk; this hold is a switch.
                                    if (talking) { onTalkCancel(); talking = false }
                                }
                            }
                            swipeFraction = null // hide the track
                            pttDrift = null
                            if (debugOverlays) {
                                println(
                                    "PTT end reason=$reason dxPx=${dx.roundToInt()} " +
                                        "dyPx=${dy.roundToInt()} thresholdPx=${swipeUpPx.roundToInt()} heldMs=$heldMs",
                                )
                            }
                            when {
                                cancelled -> {} // discarded — nothing sent, nothing transcribed
                                toggled -> onToggleHandsFree(true)
                                talking -> { onTalkStop(); talking = false }
                            }
                        }
                        else -> {} // disconnected: inert
                    }
                },
            ) {
                Box(contentAlignment = Alignment.Center) {
                    Icon(
                        when {
                            hasText -> Icons.AutoMirrored.Filled.Send
                            !pushToTalkEnabled -> Icons.Filled.Headphones
                            else -> Icons.Filled.Mic
                        },
                        contentDescription = when {
                            hasText -> "Send"
                            !pushToTalkEnabled -> "Hands-free on"
                            else -> "Hold to talk"
                        },
                    )
                }
            }
        }
      }
    }
}

/** The command tray: the argument-free "hey buddy" commands as tap buttons,
 * revealed by swiping up on the message box. Each tap fires the command (with
 * the wake prefix, so the server treats it as a control command even while
 * attached) and the caller hides the tray. Derived from COMMANDS, so it never
 * drifts from the server grammar — commands whose aliases take an argument
 * (a <name>/<dir> placeholder) are excluded since a button can't supply one. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun CommandTray(connected: Boolean, onCommand: (String) -> Unit) {
    val trayCommands = remember { COMMANDS.filter { c -> c.aliases.none { it.contains("<") } } }
    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f),
        modifier = Modifier.fillMaxWidth(),
    ) {
        FlowRow(
            Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 10.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            trayCommands.forEach { cmd ->
                OutlinedButton(
                    enabled = connected,
                    onClick = { onCommand("hey buddy " + cmd.aliases.first()) },
                ) { Text(cmd.name) }
            }
        }
    }
}

/** Live push-to-talk drift, surfaced to the debug overlay while a hold is in progress:
 *  leftward/upward drift from the touch-down point, the cancel threshold, and hold time
 *  (all in pixels / millis). */
private data class PttDrift(val dx: Float, val dy: Float, val threshold: Float, val heldMs: Long)

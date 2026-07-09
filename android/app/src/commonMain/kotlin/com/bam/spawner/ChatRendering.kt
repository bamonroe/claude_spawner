package com.bam.spawner

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.layout.widthIn
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Bolt
import androidx.compose.material.icons.filled.KeyboardArrowDown
import androidx.compose.material.icons.filled.KeyboardArrowUp
import androidx.compose.material3.Icon
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.runtime.snapshotFlow
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.TokenUsage
import com.bam.spawner.ui.MarkdownText
import kotlinx.coroutines.flow.collect
import kotlinx.coroutines.launch

/**
 * The chat message list, shared by the Android and web clients. Auto-follows the
 * newest message while parked at the bottom, but never yanks the reader down when
 * they've scrolled up (see `pinned`); offers a jump-to-latest button when unpinned.
 */
@Composable
fun ChatList(
    messages: List<ChatMessage>,
    hasMore: Boolean,
    scrollTick: Int,
    badgeMode: String,
    onLoadOlder: () -> Unit,
    modifier: Modifier,
) {
    val listState = rememberLazyListState()
    val scope = rememberCoroutineScope()
    // Bottom item index accounts for the "load older" header (item 0) when present,
    // so we land on the actual newest message, not one above it.
    val bottom = (messages.size - 1 + if (hasMore) 1 else 0).coerceAtLeast(0)
    // `pinned` tracks whether the reader is parked at the very bottom — the END of the
    // newest message is actually in view. It gates the auto-follow: if you've scrolled
    // up to read earlier messages, a new message must NOT yank you back down. It is set
    // only while the viewport is stable (see the resize block below) so an append or a
    // keyboard resize can't corrupt it.
    var pinned by remember { mutableStateOf(true) }
    // Auto-scroll to the newest message on append — but only when pinned. Keyed on the
    // LAST message so paging OLDER messages in (which doesn't change the last one) never
    // yanks the view to the bottom.
    val last = messages.lastOrNull()
    LaunchedEffect(last) {
        if (messages.isNotEmpty() && pinned) listState.animateScrollToItem(bottom)
    }
    // Explicit scroll-to-bottom (attach, typed send, read-last). Always follows, and
    // re-pins so subsequent appends resume auto-following.
    LaunchedEffect(scrollTick) {
        if (scrollTick > 0 && messages.isNotEmpty()) {
            pinned = true
            listState.animateScrollToItem(bottom)
        }
    }
    // Keep the newest message pinned above whatever sits below the list — the soft
    // keyboard and the status bars (speaking / activity / draft / mic / warm). They
    // all shrink this weighted list from the bottom (the keyboard via the outer
    // Column's imePadding() under adjustResize), and a LazyColumn does NOT follow its
    // own shrinking viewport, so the tail of the last message would slide out of view.
    //
    // We watch the viewport HEIGHT and, whenever it changes, re-pin to the newest
    // message — but only if we were parked at the bottom BEFORE the resize. Sampling
    // "am I at the bottom" AFTER the shrink is too late: a big shrink (the keyboard is
    // ~40% of the screen) has already pushed the last item out of view, so it would
    // read false and we'd wrongly skip the follow. `pinned` is therefore updated only
    // while the viewport is stable (a genuine scroll), and merely consulted — not
    // overwritten — on a resize. Snap (not animate) so the pin rides the keyboard.
    //
    // "At the bottom" here means the END of the newest message is actually visible, not
    // merely that the last item has scrolled into range — so scrolling up even a little
    // to read earlier text unpins and stops the auto-follow.
    LaunchedEffect(bottom) {
        var lastViewportH = -1
        snapshotFlow {
            val info = listState.layoutInfo
            val lastItem = info.visibleItemsInfo.lastOrNull()
            val atBottom = lastItem != null && lastItem.index >= bottom &&
                lastItem.offset + lastItem.size <= info.viewportEndOffset + 4
            info.viewportSize.height to atBottom
        }.collect { (viewportH, atBottom) ->
            if (viewportH == lastViewportH) {
                pinned = atBottom                                  // stable viewport → real scroll position
            } else {
                // Scroll to the END of the content, not the top of the last item.
                // scrollToItem(bottom, 0) aligns the item's TOP to the viewport top,
                // which for a message TALLER than the (keyboard-shrunk) viewport hides
                // its bottom half behind the keyboard. A large scrollOffset clamps to
                // max scroll, so the item's BOTTOM sits just above the keyboard/bars
                // regardless of the message's height.
                if (pinned && messages.isNotEmpty()) listState.scrollToItem(bottom, Int.MAX_VALUE)
                lastViewportH = viewportH                          // adopt the new height, keep the prior pin
            }
        }
    }
    // LazyColumn is the direct weighted child (wrapping it in a SelectionContainer
    // distorted the Column's height and pushed the input bar off-screen). Selection
    // is per-bubble instead — long-press a message to select/copy it. The Box only
    // overlays a jump-to-latest button; it keeps the same weight the LazyColumn had.
    Box(modifier) {
        LazyColumn(Modifier.fillMaxSize(), state = listState) {
            if (hasMore) item {
                TextButton(onClick = onLoadOlder, modifier = Modifier.fillMaxWidth()) {
                    Icon(Icons.Filled.KeyboardArrowUp, contentDescription = null, modifier = Modifier.size(18.dp))
                    Spacer(Modifier.width(4.dp))
                    Text("load older messages")
                }
            }
            items(messages) { Bubble(it, badgeMode) }
        }
        // When scrolled up, offer a one-tap jump back to the newest message. Sits at
        // the bottom of the chat area, just above the status bars / input bar.
        AnimatedVisibility(
            visible = !pinned && messages.isNotEmpty(),
            modifier = Modifier.align(Alignment.BottomCenter).padding(bottom = 8.dp),
        ) {
            Surface(
                onClick = {
                    pinned = true
                    scope.launch { listState.animateScrollToItem(bottom) }
                },
                shape = CircleShape,
                color = MaterialTheme.colorScheme.primary,
                contentColor = MaterialTheme.colorScheme.onPrimary,
                shadowElevation = 4.dp,
                modifier = Modifier.size(36.dp),
            ) {
                Box(contentAlignment = Alignment.Center) {
                    Icon(Icons.Filled.KeyboardArrowDown, contentDescription = "Scroll to latest")
                }
            }
        }
    }
}

@Composable
fun Bubble(msg: ChatMessage, badgeMode: String = "off") {
    val user = msg.role == Role.USER
    val dark = MaterialTheme.colorScheme.background.luminance() < 0.5f
    val bg = when (msg.role) {
        Role.USER -> MaterialTheme.colorScheme.primary
        Role.CLAUDE -> MaterialTheme.colorScheme.surfaceVariant
        Role.SYSTEM -> if (dark) Color(0xFF9A5B12) else Color(0xFFFFE0B2) // amber — "bud", not Claude
    }
    val fg = when (msg.role) {
        Role.USER -> MaterialTheme.colorScheme.onPrimary
        Role.CLAUDE -> MaterialTheme.colorScheme.onSurfaceVariant
        Role.SYSTEM -> if (dark) Color.White else Color(0xFF7A4A00)
    }
    Row(
        Modifier.fillMaxWidth().padding(horizontal = 8.dp, vertical = 3.dp),
        horizontalArrangement = if (user) Arrangement.End else Arrangement.Start,
    ) {
        Surface(color = bg, contentColor = fg, shape = RoundedCornerShape(14.dp), modifier = Modifier.widthIn(max = 320.dp)) {
            Column {
                // Per-bubble selection so the text is long-press copyable, without a
                // list-wide SelectionContainer (which distorted the Column layout).
                SelectionContainer {
                    if (msg.role == Role.CLAUDE) {
                        MarkdownText(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp))
                    } else {
                        Text(msg.text, Modifier.padding(horizontal = 12.dp, vertical = 8.dp), style = MaterialTheme.typography.bodyMedium)
                    }
                }
                // Per-turn token badge under Claude replies (Appearance → Token badge).
                if (msg.role == Role.CLAUDE && badgeMode != "off") msg.usage?.let { TokenBadge(it, badgeMode) }
                // Date/time badge below the token line: bottom-right for Claude, bottom-left
                // for user input. Only live messages carry a timestamp (history has ts=0).
                if (msg.ts > 0) Text(
                    fmtStamp(msg.ts),
                    style = MaterialTheme.typography.labelSmall,
                    color = fg.copy(alpha = 0.5f),
                    modifier = Modifier
                        .align(if (user) Alignment.End else Alignment.Start)
                        .padding(start = 12.dp, end = 12.dp, bottom = 6.dp),
                )
            }
        }
    }
}

// TokenBadge renders one turn's token usage as a small caption under a reply. The
// ⚡ marks a warm prompt-cache hit; detailed mode breaks the context into fresh
// input / cached / newly-cached tokens. See docs/protocol.md's `output.usage`.
@Composable
fun TokenBadge(u: TokenUsage, mode: String) {
    val label = if (mode == "detailed") buildString {
        append("${fmtTok(u.input)} in")
        if (u.cacheRead > 0) append(" · ${fmtTok(u.cacheRead)} cached")
        if (u.cacheWrite > 0) append(" · ${fmtTok(u.cacheWrite)} new")
        append(" · ${fmtTok(u.output)} out")
    } else {
        "${fmtTok(u.contextTokens)}↑ ${fmtTok(u.output)}↓"
    }
    Row(
        verticalAlignment = Alignment.CenterVertically,
        modifier = Modifier.padding(start = 12.dp, end = 12.dp, bottom = 6.dp),
    ) {
        Text(
            label,
            style = MaterialTheme.typography.labelSmall,
            color = LocalContentColor.current.copy(alpha = 0.6f),
        )
        // ⚡ warm prompt-cache hit marker.
        if (u.warmHit) {
            Spacer(Modifier.width(2.dp))
            Icon(
                Icons.Filled.Bolt, contentDescription = "warm cache hit",
                tint = LocalContentColor.current.copy(alpha = 0.6f), modifier = Modifier.size(12.dp),
            )
        }
    }
}

/** fmtStamp formats a unix-seconds timestamp as a compact local date/time badge.
 *  Platform-specific: Android uses SimpleDateFormat, the browser uses Intl via JS. */
expect fun fmtStamp(unixSeconds: Long): String

// fmtTok renders a token count compactly: 800, 1.2k, 24k. (Multiplatform — no
// JVM String.format; the sub-10k branch rounds to one decimal via integer math.)
fun fmtTok(n: Int): String = when {
    n >= 10_000 -> "${(n + 500) / 1000}k"
    n >= 1_000 -> {
        val tenths = (n + 50) / 100 // nearest 0.1k, in tenths-of-a-k
        "${tenths / 10}.${tenths % 10}k"
    }
    else -> n.toString()
}

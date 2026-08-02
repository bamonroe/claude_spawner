package com.bam.spawner

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.NotificationsActive
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.ServerMsg

/**
 * The `notice` banner: news from a session you are NOT looking at — today, a background
 * job that finished and was narrated while nothing was attached.
 *
 * It sits above the chat rather than in it, in "buddy orange" (the same attention cue the
 * sidebar uses for unread sessions), because the news belongs to a *different* session and
 * must never be mistaken for a message in the one on screen. Tapping the body opens that
 * session; the ✕ dismisses it. One banner per session, newest last — [NoticeStore] enforces
 * that, so a chatty session replaces its banner instead of stacking them.
 */
@Composable
fun NoticeBanner(notice: ServerMsg.Notice, onOpen: () -> Unit, onDismiss: () -> Unit) {
    Surface(color = BuddyOrange, contentColor = Color.White, modifier = Modifier.fillMaxWidth()) {
        Row(
            modifier = Modifier.clickable(onClick = onOpen).padding(start = 12.dp, top = 6.dp, bottom = 6.dp),
            verticalAlignment = Alignment.CenterVertically,
        ) {
            Icon(Icons.Filled.NotificationsActive, contentDescription = null, modifier = Modifier.size(16.dp))
            Spacer(Modifier.width(8.dp))
            Column(Modifier.weight(1f)) {
                Text(notice.name, style = MaterialTheme.typography.labelLarge)
                Text(
                    notice.text, style = MaterialTheme.typography.bodySmall,
                    maxLines = 3, overflow = TextOverflow.Ellipsis,
                )
            }
            IconButton(onClick = onDismiss) {
                Icon(Icons.Filled.Close, contentDescription = "Dismiss", modifier = Modifier.size(18.dp))
            }
        }
    }
}

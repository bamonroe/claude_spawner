package com.bam.spawner

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.BarChart
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material.icons.filled.Edit
import androidx.compose.material.icons.filled.Inventory2
import androidx.compose.material.icons.filled.Refresh
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.DiscoveredInfo
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageEstimateInfo

// The loopback host name. To the server, localhost is just another SSH host —
// dialed over loopback SSH using the server's SSH defaults — not a special implicit
// default, so the app always names it explicitly and lists it like any other host.
// A deployment whose server can't reach its own box simply never picks Local.
const val LOCAL_HOST = "localhost"

/** The sessions drawer: discovered sessions grouped by host, pull-to-refresh, detach, and the
 *  usage readouts pinned to the bottom. Fully parameterized so it renders on both clients. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun Sidebar(
    discovered: List<DiscoveredInfo>,
    discoverError: String,
    attached: String?,
    attachedId: String,
    onNew: () -> Unit,
    refreshing: Boolean,
    onRefresh: () -> Unit,
    onOpen: (DiscoveredInfo) -> Unit,
    onRename: (DiscoveredInfo) -> Unit,
    onDelete: (DiscoveredInfo) -> Unit,
    onDetach: () -> Unit,
    rateLimit: RateLimitInfo?,
    usageEstimate: UsageEstimateInfo?,
    onCheckUsage: () -> Unit,
) {
    Column(Modifier.fillMaxHeight().statusBarsPadding().navigationBarsPadding().padding(12.dp)) {
        Text("Sessions", style = MaterialTheme.typography.titleLarge)
        Row {
            TextButton(onClick = onNew) {
                Icon(Icons.Filled.Add, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(4.dp))
                Text("New")
            }
            // A visible refresh control alongside the pull-to-refresh gesture, so
            // mouse/desktop users can re-scan sessions without a drag.
            TextButton(onClick = onRefresh, enabled = !refreshing) {
                Icon(Icons.Filled.Refresh, contentDescription = null, modifier = Modifier.size(18.dp))
                Spacer(Modifier.width(4.dp))
                Text("Refresh")
            }
        }
        if (discoverError.isNotBlank()) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Icons.Filled.Warning, contentDescription = null,
                    tint = MaterialTheme.colorScheme.error, modifier = Modifier.size(16.dp))
                Spacer(Modifier.width(4.dp))
                Text(discoverError, color = MaterialTheme.colorScheme.error,
                    style = MaterialTheme.typography.bodySmall)
            }
        }
        HorizontalDivider()
        // Pull down anywhere on the list to refresh; it also auto-refreshes on open.
        PullToRefreshBox(
            isRefreshing = refreshing,
            onRefresh = onRefresh,
            modifier = Modifier.weight(1f),
        ) {
        LazyColumn(Modifier.fillMaxSize()) {
            // Group sessions by the host they run on; localhost first, then the rest
            // alphabetically. Each group gets a header.
            val grouped = discovered.groupBy { it.host.ifBlank { LOCAL_HOST } }
            val hostsInOrder = grouped.keys.sortedWith(compareBy({ it != LOCAL_HOST }, { it }))
            hostsInOrder.forEach { host ->
                item {
                    Text(
                        host,
                        style = MaterialTheme.typography.labelLarge,
                        color = MaterialTheme.colorScheme.primary,
                        modifier = Modifier.fillMaxWidth().padding(top = 10.dp, bottom = 2.dp),
                    )
                    HorizontalDivider()
                }
                items(grouped[host].orEmpty()) { d ->
                // Highlight the attached row by stable id, not name — the same session
                // can be named differently here than when we attached (e.g. server switch).
                val isAttached = d.registered && attachedId.isNotEmpty() && d.sessionId == attachedId
                Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f).clickable { onOpen(d) }) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            if (d.busy) {
                                Icon(Icons.Filled.Settings, null, Modifier.size(14.dp))
                                Spacer(Modifier.width(4.dp))
                            } else if (d.active) {
                                Icon(Icons.Filled.Warning, null, Modifier.size(14.dp))
                                Spacer(Modifier.width(4.dp))
                            }
                            Text(d.name, style = MaterialTheme.typography.titleSmall,
                                color = if (isAttached) MaterialTheme.colorScheme.primary else Color.Unspecified,
                                fontWeight = if (isAttached) FontWeight.Bold else null)
                            if (d.target == "sandbox") Row(
                                Modifier.padding(start = 6.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Icon(Icons.Filled.Inventory2, contentDescription = null,
                                    tint = MaterialTheme.colorScheme.tertiary, modifier = Modifier.size(14.dp))
                                Spacer(Modifier.width(4.dp))
                                Text("sandbox",
                                    style = MaterialTheme.typography.labelSmall,
                                    color = MaterialTheme.colorScheme.tertiary)
                            }
                        }
                        Text(d.dir, style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.outline, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        val parts = mutableListOf<String>()
                        if (d.busy) parts.add("working…")
                        if (isAttached) parts.add("attached")
                        if (d.active) parts.add("live in terminal")
                        else relativeTime(d.lastActive).let { if (it.isNotEmpty()) parts.add(it) }
                        if (parts.isNotEmpty()) Text(parts.joinToString(" · "),
                            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                    }
                    Icon(Icons.Filled.Edit, contentDescription = "Rename",
                        modifier = Modifier.clickable { onRename(d) }.padding(8.dp).size(20.dp))
                    Icon(Icons.Filled.Delete, contentDescription = "Delete",
                        modifier = Modifier.clickable { onDelete(d) }.padding(8.dp).size(20.dp))
                }
                HorizontalDivider()
                }
            }
        }
        }
        if (attached != null) {
            HorizontalDivider()
            TextButton(onClick = onDetach) { Text("Detach from $attached") }
        }
        // Usage readouts pinned to the bottom of the drawer: the drift-live estimate
        // (nudges each turn, snaps on /usage), the coarse session-limit reset, and
        // "Check usage" to run `/usage` on demand for the exact numbers.
        HorizontalDivider()
        usageEstimate?.takeIf { it.calibrated }?.let { UsageEstimateLine(it) }
        rateLimit?.let { SessionLimitFooter(it) }
        TextButton(onClick = onCheckUsage) {
            Icon(Icons.Filled.BarChart, contentDescription = null, modifier = Modifier.size(16.dp))
            Spacer(Modifier.width(4.dp))
            Text("Check usage")
        }
    }
}

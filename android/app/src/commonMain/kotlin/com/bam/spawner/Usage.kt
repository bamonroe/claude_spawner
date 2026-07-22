package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.HourglassEmpty
import androidx.compose.material.icons.filled.Warning
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.RateLimitInfo
import com.bam.spawner.net.UsageReport

/** "· in 2h 13m" until a future unix-seconds reset (empty if past/now). */
fun relResetSuffix(unixSeconds: Long): String {
    val secs = unixSeconds - nowEpochSeconds()
    if (secs <= 0) return ""
    val h = secs / 3600; val m = (secs % 3600) / 60
    return when {
        h > 0 -> " · in ${h}h ${m}m"
        m > 0 -> " · in ${m}m"
        else -> " · soon"
    }
}

/** Coarse "2h ago" / "3d ago" from a unix-seconds timestamp. */
fun relativeTime(unixSeconds: Long): String {
    if (unixSeconds <= 0) return ""
    val secs = nowEpochSeconds() - unixSeconds
    return when {
        secs < 60 -> "just now"
        secs < 3600 -> "${secs / 60}m ago"
        secs < 86400 -> "${secs / 3600}h ago"
        secs < 86400 * 30 -> "${secs / 86400}d ago"
        else -> "${secs / (86400 * 30)}mo ago"
    }
}

/** Coarse session/weekly rate-limit reset footer (from the server's rate-limit signal). */
@Composable
fun SessionLimitFooter(info: RateLimitInfo) {
    val warn = !info.allowed
    val window = when {
        info.limitType == "five_hour" -> "5-hour session"
        info.limitType.contains("week") -> "weekly"
        info.limitType.isBlank() -> "usage"
        else -> info.limitType
    }
    val reset = if (info.resetsAt > 0) "resets ${fmtClock(info.resetsAt)}${relResetSuffix(info.resetsAt)}" else ""
    Column(Modifier.padding(vertical = 4.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Icon(
                if (warn) Icons.Filled.Warning else Icons.Filled.HourglassEmpty,
                contentDescription = null,
                tint = if (warn) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
                modifier = Modifier.size(16.dp),
            )
            Spacer(Modifier.width(4.dp))
            Text(
                "Claude $window limit",
                style = MaterialTheme.typography.labelMedium,
                color = if (warn) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface,
                fontWeight = if (warn) FontWeight.Bold else null,
            )
        }
        if (reset.isNotEmpty()) Text(reset, style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline)
        if (warn && info.status.isNotBlank()) Text("status: ${info.status}",
            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.error)
        if (info.usingOverage) Text("using overage credits",
            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
    }
}

/** One labeled percent-used row with a progress bar and reset time. */
@Composable
fun UsageBar(label: String, pct: Int, reset: String) {
    val known = pct in 0..100
    Column {
        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
            Text(label, style = MaterialTheme.typography.titleSmall)
            Text(if (known) "$pct% used" else "—", style = MaterialTheme.typography.titleSmall,
                color = if (pct >= 90) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary)
        }
        if (known) LinearProgressIndicator(
            progress = { pct / 100f },
            modifier = Modifier.fillMaxWidth().padding(top = 4.dp),
        )
        if (reset.isNotBlank()) Text("resets $reset", style = MaterialTheme.typography.labelSmall,
            color = MaterialTheme.colorScheme.outline)
    }
}

/** The full `/usage` sheet: session/week bars and the raw breakdown. */
@Composable
fun UsageSheet(
    loading: Boolean, report: UsageReport?, onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = { TextButton(onClick = onDismiss) { Text("Close") } },
        title = { Text("Claude usage") },
        text = {
            Column(Modifier.verticalScroll(rememberScrollState())) {
                when {
                    report == null -> Row(verticalAlignment = Alignment.CenterVertically) {
                        CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
                        Spacer(Modifier.width(10.dp))
                        Text("Checking usage…")
                    }
                    report.sessionPct < 0 && report.weekPct < 0 -> // parse failed — show raw
                        Text(report.text.ifBlank { "No usage data." }, style = MaterialTheme.typography.bodySmall)
                    else -> {
                        UsageBar("Session", report.sessionPct, report.sessionReset)
                        Spacer(Modifier.height(12.dp))
                        UsageBar("This week", report.weekPct, report.weekReset)
                        val idx = report.text.indexOf("What's contributing")
                        if (idx >= 0) {
                            Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                            Text(report.text.substring(idx), style = MaterialTheme.typography.bodySmall,
                                color = MaterialTheme.colorScheme.outline)
                        }
                    }
                }
            }
        },
    )
}

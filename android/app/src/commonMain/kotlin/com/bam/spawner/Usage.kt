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
import androidx.compose.material.icons.filled.BarChart
import androidx.compose.material.icons.filled.Calculate
import androidx.compose.material.icons.filled.HourglassEmpty
import androidx.compose.material.icons.filled.Place
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
import com.bam.spawner.net.UsageEstimateInfo
import com.bam.spawner.net.UsageReport
import kotlin.math.roundToInt

/** Percent as "47%", or "—" when unknown (−1). */
fun pctStr(p: Double): String = if (p < 0) "—" else "${p.roundToInt()}%"

/** Compact token count for large sums: 800, 1.2k, 24k, 3.4M. */
fun fmtTokL(n: Long): String = when {
    n >= 10_000_000 -> "${(n + 500_000) / 1_000_000}M"
    n >= 1_000_000 -> oneDecimal(n, 1_000_000) + "M"
    n >= 10_000 -> "${(n + 500) / 1000}k"
    n >= 1_000 -> oneDecimal(n, 1000) + "k"
    else -> n.toString()
}

/** n/div to one rounded decimal place, without JVM String.format (e.g. 1_500_000,1_000_000 → "1.5"). */
private fun oneDecimal(n: Long, div: Long): String {
    val tenths = (n * 10 + div / 2) / div
    return "${tenths / 10}.${tenths % 10}"
}

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

/** The drift-live usage estimate line (session / week percentages). */
@Composable
fun UsageEstimateLine(e: UsageEstimateInfo) {
    Row(Modifier.padding(vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
        Icon(
            Icons.Filled.BarChart, contentDescription = null,
            tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(16.dp),
        )
        Spacer(Modifier.width(4.dp))
        Text(
            "Session ~${pctStr(e.sessionEstPct)} · Week ~${pctStr(e.weekEstPct)} (est)",
            style = MaterialTheme.typography.labelMedium,
            color = MaterialTheme.colorScheme.primary,
        )
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

/** The full `/usage` sheet: session/week bars, live estimate, two-point calibration, raw breakdown. */
@Composable
fun UsageSheet(
    loading: Boolean, report: UsageReport?, estimate: UsageEstimateInfo?,
    onSet: () -> Unit, onCalc: () -> Unit, onDismiss: () -> Unit,
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
                        // The running server-wide estimate: what it had drifted to (all
                        // sessions/clients) just before this check snapped it back.
                        estimate?.takeIf { it.calibrated }?.let { e ->
                            Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                            Text("Live estimate (all sessions/clients, drifts each turn)",
                                style = MaterialTheme.typography.labelMedium)
                            Text("Session ~${pctStr(e.sessionEstPct)} · Week ~${pctStr(e.weekEstPct)}",
                                style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.primary)
                            Text("odometer: ${fmtTokL(e.cumTokens)} tokens · +${e.turnsSinceCheck} turns since last check",
                                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        }
                        // Manual two-point rate calibration. "Set" marks the current
                        // odometer/percentages; after burning enough tokens to move a few
                        // whole percent, "Calc" sets tokens-per-percent directly from that
                        // interval — no EMA, so it beats the passive check's rounding bias.
                        Spacer(Modifier.height(12.dp)); HorizontalDivider(); Spacer(Modifier.height(8.dp))
                        Text("Calibrate max (two-point)", style = MaterialTheme.typography.labelMedium)
                        estimate?.takeIf { it.benchSet }?.let { e ->
                            Text("benchmark: ${pctStr(e.benchSessPct)} session · ${pctStr(e.benchWeekPct)} week · +${fmtTokL(e.tokensSinceSet)} tokens since",
                                style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        } ?: Text("no benchmark set — tap Set, burn a few % of tokens, then Calc",
                            style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.outline)
                        Row {
                            TextButton(onClick = onSet) {
                                Icon(Icons.Filled.Place, contentDescription = null, modifier = Modifier.size(16.dp))
                                Spacer(Modifier.width(4.dp))
                                Text("Set")
                            }
                            TextButton(onClick = onCalc) {
                                Icon(Icons.Filled.Calculate, contentDescription = null, modifier = Modifier.size(16.dp))
                                Spacer(Modifier.width(4.dp))
                                Text("Calc max")
                            }
                        }
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

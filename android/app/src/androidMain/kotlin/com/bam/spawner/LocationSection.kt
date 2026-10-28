package com.bam.spawner

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.core.content.ContextCompat

/*
 * Android-only UI for "Tag recordings with location": the rationale dialog, the
 * runtime permission request, and the Settings › Audio switch. The fix itself is
 * kept by location/LocationTracker; this file only asks for and records consent.
 */

private val LOCATION_PERMISSIONS = arrayOf(
    Manifest.permission.ACCESS_FINE_LOCATION,
    Manifest.permission.ACCESS_COARSE_LOCATION,
)

private const val RATIONALE =
    "Every voice clip is archived on your spawner server. With this on, the app attaches " +
        "the phone's most recent location fix to each clip so you can later find recordings " +
        "by where you made them. It uses low-power location only (no extra GPS), never " +
        "delays a recording waiting for a fix, and the location goes only to your own server. " +
        "Approximate location is enough; you can turn this off any time in Settings › Audio."

@Composable
private fun LocationRationaleDialog(onAllow: () -> Unit, onDecline: () -> Unit) {
    AlertDialog(
        onDismissRequest = onDecline,
        title = { Text("Tag recordings with location?") },
        text = { Text(RATIONALE) },
        confirmButton = { TextButton(onClick = onAllow) { Text("Continue") } },
        dismissButton = { TextButton(onClick = onDecline) { Text("Don't tag") } },
    )
}

/**
 * Shared consent flow: shows the rationale, then the system prompt. [onResult] gets
 * whether coarse or fine location ended up granted. Returns a `start()` trigger.
 */
@Composable
private fun rememberLocationConsent(onResult: (Boolean) -> Unit): Pair<() -> Unit, @Composable () -> Unit> {
    var showRationale by remember { mutableStateOf(false) }
    val request = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { res ->
        onResult(res.values.any { it })
    }
    val dialog: @Composable () -> Unit = {
        if (showRationale) LocationRationaleDialog(
            onAllow = { showRationale = false; request.launch(LOCATION_PERMISSIONS) },
            onDecline = { showRationale = false; onResult(false) },
        )
    }
    return ({ showRationale = true }) to dialog
}

@Composable
private fun hasLocationPermission(): Boolean {
    val ctx = LocalContext.current
    return LOCATION_PERMISSIONS.any {
        ContextCompat.checkSelfPermission(ctx, it) == PackageManager.PERMISSION_GRANTED
    }
}

/**
 * First-launch prompt: tagging defaults on, so ask once (rationale first). A decline
 * turns the setting off; it's never re-asked at launch — the settings switch re-asks.
 */
@Composable
fun LocationStartupPrompt(settings: SettingsStore, onChanged: () -> Unit) {
    val granted = hasLocationPermission()
    val (ask, dialog) = rememberLocationConsent { ok ->
        if (!ok) settings.tagLocation = false
        onChanged()
    }
    LaunchedEffect(Unit) {
        if (settings.tagLocation && !granted && !settings.locationPrompted) {
            settings.locationPrompted = true
            ask()
        }
    }
    dialog()
}

/** Settings › Audio section: the "Tag recordings with location" switch. Turning it on
 *  without the permission runs the consent flow; a denial leaves it off. */
@Composable
fun LocationSection(settings: SettingsStore, onChanged: () -> Unit) {
    val granted = hasLocationPermission()
    var on by remember { mutableStateOf(settings.tagLocation && granted) }
    var denied by remember { mutableStateOf(false) }
    val (ask, dialog) = rememberLocationConsent { ok ->
        on = ok; settings.tagLocation = ok; denied = !ok
        onChanged()
    }
    HorizontalDivider()
    Row(verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text("Tag recordings with location", style = MaterialTheme.typography.titleMedium)
            Text(
                "Attach the phone's latest location fix to each voice clip the server archives, " +
                    "so you can find recordings by where you made them. Low-power only; never " +
                    "delays a recording.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
            )
            if (denied) Text(
                "Location permission was not granted. If Android stops asking, allow it under " +
                    "App info › Permissions › Location.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error,
            )
        }
        Switch(checked = on, onCheckedChange = { want ->
            if (want && !granted) {
                ask()
            } else {
                on = want; settings.tagLocation = want; denied = false
                onChanged()
            }
        })
    }
    dialog()
}

package com.bam.spawner

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Description
import androidx.compose.material.icons.filled.Folder
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp

/** Where a transfer starts: the attached session's directory and host, or the host
 *  root when nothing is attached / discovery hasn't surfaced it. Shared by both the
 *  Android and web transfer buttons. */
data class DirHost(val dir: String, val host: String)

/** A host-scoped filesystem picker for file transfer, reusing the `browse`/`listing`
 *  protocol. In directory mode (pickFiles = false) folders are navigable and a confirm
 *  button selects the current directory; in file mode (pickFiles = true) the listing
 *  also shows regular files and tapping one selects it. [onPick] receives the chosen
 *  absolute path. The displayed entries and the confirmed directory are kept in lockstep
 *  by only rendering the listing once it matches the directory we asked for.
 *
 *  Lives in commonMain (typed against [AppController]) so the Android SAF button and the
 *  browser file button share one picker. Glyphs are Material icons, not emoji, so the
 *  folder/file rows render in the browser too (Skiko ships no emoji font). */
@Composable
fun TransferPickerDialog(
    controller: AppController,
    host: String,
    startDir: String,
    pickFiles: Boolean,
    title: String,
    onPick: (String) -> Unit,
    onDismiss: () -> Unit,
) {
    var dir by remember { mutableStateOf(startDir) }
    val listing by controller.listing.collectAsState()
    LaunchedEffect(Unit) { controller.browse(startDir, host, pickFiles) }
    // Only trust the listing when it's the answer to our current directory — otherwise
    // a stale listing (from the New-session browser, or a slower nav) would mislabel
    // the confirm target.
    val current = listing?.takeIf { it.path == dir }
    fun go(target: String) { dir = target; controller.browse(target, host, pickFiles) }

    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text(title) },
        text = {
            Column {
                Text(dir, style = MaterialTheme.typography.labelSmall, maxLines = 1)
                LazyColumn(Modifier.heightIn(max = 360.dp)) {
                    if (dir != "/") item {
                        EntryRow(
                            icon = Icons.Filled.Folder, label = "..",
                            onClick = { go(current?.parent?.ifEmpty { "/" } ?: "/") },
                        )
                    }
                    items(current?.entries ?: emptyList()) { e ->
                        EntryRow(
                            icon = if (e.dir) Icons.Filled.Folder else Icons.Filled.Description,
                            label = e.name,
                            onClick = { if (e.dir) go(e.path) else if (pickFiles) onPick(e.path) },
                        )
                    }
                }
            }
        },
        confirmButton = {
            if (!pickFiles) TextButton(onClick = { onPick(dir) }) { Text("Upload here") }
            else TextButton(onClick = onDismiss) { Text("Close") }
        },
        dismissButton = {
            if (!pickFiles) TextButton(onClick = onDismiss) { Text("Cancel") }
        },
    )
}

/** One tappable row in the transfer picker: a leading folder/file icon and the name. */
@Composable
private fun EntryRow(
    icon: androidx.compose.ui.graphics.vector.ImageVector,
    label: String,
    onClick: () -> Unit,
) {
    Row(
        Modifier.fillMaxWidth().clickable(onClick = onClick).padding(vertical = 12.dp),
        verticalAlignment = Alignment.CenterVertically,
    ) {
        Icon(icon, contentDescription = null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Spacer(Modifier.width(10.dp))
        Text(label)
    }
}

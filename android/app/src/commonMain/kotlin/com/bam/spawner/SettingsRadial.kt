package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.AssistChip
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Switch
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.layout.padding
import androidx.compose.ui.unit.dp

/**
 * Settings → Radial menu: edit the tree the double-tap ring shows.
 *
 * The editor mirrors the menu itself — you drill into a submenu here the same way you'd
 * open it there, so what you're editing is always one ring. Every change writes straight
 * back to [Prefs.radialMenu]; the ring reads it fresh each time it opens.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun RadialMenuSettings(settings: Prefs, onBack: () -> Unit) {
    var config by remember { mutableStateOf(parseRadialMenu(settings.radialMenu)) }
    // Index path to the submenu being edited; empty = the root ring.
    var path by remember { mutableStateOf(listOf<Int>()) }
    var adding by remember { mutableStateOf(false) }
    var renaming by remember { mutableStateOf<Int?>(null) }
    val save = { c: RadialMenuConfig -> config = c; settings.radialMenu = encodeRadialMenu(c) }

    val items = config.itemsAt(path)
    SettingsScaffold("Radial menu", onBack) {
        Text(
            "Double-tap the chat log to open the ring. A submenu opens in place — the ring " +
                "stays exactly where it bloomed and only its contents change.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )

        if (path.isEmpty()) {
            Text("Centre button", style = MaterialTheme.typography.titleMedium)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                RadialActions.all.forEach { (id, label) ->
                    FilterChip(
                        selected = config.center == id,
                        onClick = { save(config.copy(center = id)) },
                        label = { Text(label) },
                    )
                }
            }
            HorizontalDivider()
        } else {
            // The trail of submenu names, so you know which ring you're editing.
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedButton(onClick = { path = path.dropLast(1) }) { Text("Up") }
                Text(config.pathLabel(path), style = MaterialTheme.typography.titleMedium)
            }
            Text(
                "Inside a submenu the ring's centre becomes Back, and back/Escape pops one level.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
            )
        }

        if (items.isEmpty()) {
            Text("No items — this ring would be empty.", style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.outline)
        }
        items.forEachIndexed { i, item ->
            Surface(
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                shape = RoundedCornerShape(12.dp),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Text(item.label, style = MaterialTheme.typography.titleMedium)
                    Text(item.describe(), style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.outline)
                    // An expanded dynamic ring has no button of its own — its entries are
                    // spliced into this one — so it can't have a submenu behind it either.
                    if (item is RadialItem.Dynamic) {
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Switch(checked = item.expand, onCheckedChange = { on ->
                                save(config.replaceAt(path, i, item.copy(expand = on)))
                            })
                            Text("  Splice into this ring instead of a submenu",
                                style = MaterialTheme.typography.bodySmall)
                        }
                    }
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        if (item is RadialItem.Submenu) {
                            AssistChip(onClick = { path = path + i }, label = { Text("Open") })
                        }
                        AssistChip(onClick = { renaming = i }, label = { Text("Rename") })
                        AssistChip(
                            onClick = { save(config.moveAt(path, i, -1)) },
                            label = { Text("Up") },
                        )
                        AssistChip(
                            onClick = { save(config.moveAt(path, i, 1)) },
                            label = { Text("Down") },
                        )
                        AssistChip(onClick = { save(config.removeAt(path, i)) }, label = { Text("Remove") })
                    }
                }
            }
        }
        if (items.size >= PALETTE_SLOTS) {
            Text("A ring shows at most $PALETTE_SLOTS items; the rest are hidden.",
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }
        Button(onClick = { adding = true }, modifier = Modifier.fillMaxWidth()) { Text("Add item") }
        OutlinedButton(
            onClick = { path = emptyList(); save(DefaultRadialMenu) },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Reset to default") }
    }

    if (adding) {
        AddRadialItemDialog(
            onDismiss = { adding = false },
            onAdd = { item -> save(config.addAt(path, item)); adding = false },
        )
    }
    renaming?.let { i ->
        val item = items.getOrNull(i)
        if (item == null) renaming = null else RenameDialog(item.label, onDismiss = { renaming = null }) { name ->
            save(config.replaceAt(path, i, item.withLabel(name)))
            renaming = null
        }
    }
}

/** Pick what a new slot is: a built-in action, a submenu, or a live-state ring. */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun AddRadialItemDialog(onDismiss: () -> Unit, onAdd: (RadialItem) -> Unit) {
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
        title = { Text("Add item") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Submenu", style = MaterialTheme.typography.titleSmall)
                AssistChip(
                    onClick = { onAdd(RadialItem.Submenu("Submenu")) },
                    label = { Text("Empty submenu") },
                )
                Text("Live", style = MaterialTheme.typography.titleSmall)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    RadialSources.all.forEach { (id, label) ->
                        AssistChip(onClick = { onAdd(RadialItem.Dynamic(label, id)) }, label = { Text(label) })
                    }
                }
                Text("Actions", style = MaterialTheme.typography.titleSmall)
                FlowRow(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    RadialActions.all.forEach { (id, label) ->
                        AssistChip(onClick = { onAdd(RadialItem.Action(label, id)) }, label = { Text(label) })
                    }
                }
            }
        },
    )
}

/** Slot labels sit in a 76.dp circle, so they're the user's to keep short. */
@Composable
private fun RenameDialog(current: String, onDismiss: () -> Unit, onSave: (String) -> Unit) {
    var text by remember { mutableStateOf(current) }
    AlertDialog(
        onDismissRequest = onDismiss,
        confirmButton = { TextButton(onClick = { if (text.isNotBlank()) onSave(text.trim()) }) { Text("Save") } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
        title = { Text("Label") },
        text = { OutlinedTextField(text, { text = it }, singleLine = true, label = { Text("Shown on the ring") }) },
    )
}

private fun RadialItem.describe(): String = when (this) {
    is RadialItem.Action -> "Action · ${RadialActions.label(action)}"
    is RadialItem.Submenu -> "Submenu · ${items.size} item(s)"
    is RadialItem.Dynamic -> "Live · ${RadialSources.label(source)}"
}

private fun RadialItem.withLabel(label: String): RadialItem = when (this) {
    is RadialItem.Action -> copy(label = label)
    is RadialItem.Submenu -> copy(label = label)
    is RadialItem.Dynamic -> copy(label = label)
}

// --- Tree edits by index path. Each returns a new config; an unreachable path is a no-op.

private fun RadialMenuConfig.itemsAt(path: List<Int>): List<RadialItem> {
    var list = items
    for (i in path) list = (list.getOrNull(i) as? RadialItem.Submenu)?.items ?: return emptyList()
    return list
}

private fun RadialMenuConfig.pathLabel(path: List<Int>): String {
    val names = mutableListOf<String>()
    var list = items
    for (i in path) {
        val sub = list.getOrNull(i) as? RadialItem.Submenu ?: break
        names += sub.label
        list = sub.items
    }
    return names.joinToString(" › ")
}

private fun RadialMenuConfig.mapAt(path: List<Int>, edit: (List<RadialItem>) -> List<RadialItem>) =
    copy(items = mapList(items, path, edit))

private fun mapList(list: List<RadialItem>, path: List<Int>, edit: (List<RadialItem>) -> List<RadialItem>): List<RadialItem> {
    if (path.isEmpty()) return edit(list)
    val i = path.first()
    val sub = list.getOrNull(i) as? RadialItem.Submenu ?: return list
    return list.toMutableList().also { it[i] = sub.copy(items = mapList(sub.items, path.drop(1), edit)) }
}

private fun RadialMenuConfig.addAt(path: List<Int>, item: RadialItem) = mapAt(path) { it + item }

private fun RadialMenuConfig.removeAt(path: List<Int>, i: Int) =
    mapAt(path) { l -> l.filterIndexed { j, _ -> j != i } }

private fun RadialMenuConfig.replaceAt(path: List<Int>, i: Int, item: RadialItem) =
    mapAt(path) { l -> l.toMutableList().also { if (i in it.indices) it[i] = item } }

private fun RadialMenuConfig.moveAt(path: List<Int>, i: Int, delta: Int) = mapAt(path) { l ->
    val j = i + delta
    if (i !in l.indices || j !in l.indices) l
    else l.toMutableList().also { it[i] = l[j]; it[j] = l[i] }
}

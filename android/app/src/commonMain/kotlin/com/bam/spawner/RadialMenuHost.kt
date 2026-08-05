package com.bam.spawner

import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.geometry.Offset

/** A dynamic source's entries: the slot to draw plus what tapping it should do. */
data class RadialDynamicEntry(val slot: PaletteSlot, val onPick: () -> Unit)

/**
 * Drives a configured [RadialMenuConfig] through one [RadialPalette].
 *
 * Descending into a submenu swaps the ring's contents while leaving [origin] — and so the
 * whole geometry — untouched: the menu doesn't move, it changes. Inside a submenu the
 * centre becomes "Back" (one level up) and back/Escape/scrim pop a level instead of
 * closing, so the ring only disappears from the root.
 *
 * [dynamic] resolves a [RadialItem.Dynamic]'s source to live entries; [onAction] runs a
 * built-in ([RadialActions]). Both are the caller's, so this stays free of app state.
 */
@Composable
fun RadialMenuHost(
    config: RadialMenuConfig,
    origin: Offset?,
    dynamic: (source: String) -> List<RadialDynamicEntry>,
    onAction: (action: String) -> Unit,
    onDismiss: () -> Unit,
    centerHighlighted: Boolean = false,
) {
    // The path from the root to the ring on screen. Empty = the root ring.
    var path by remember { mutableStateOf(listOf<RadialItem>()) }
    val here = path.lastOrNull()

    // Tapping a leaf runs it and closes; tapping a branch descends in place.
    val pick: (RadialItem) -> Unit = { item ->
        when (item) {
            is RadialItem.Action -> onAction(item.action)
            else -> path = path + item
        }
    }
    // What the current ring shows. A dynamic node's entries are resolved on the spot, so
    // the session list is always the live one rather than whatever it was when we opened.
    val items: List<RadialItem>? = when (here) {
        null -> config.items
        is RadialItem.Submenu -> here.items
        else -> null
    }
    val entries: List<RadialDynamicEntry> =
        items?.flatMapIndexed { i, item -> expand(i, item, dynamic, pick) }
            ?: (here as? RadialItem.Dynamic)?.let { dynamic(it.source) }
            ?: emptyList()
    val pop = { path = path.dropLast(1) }

    RadialPalette(
        slots = entries.map { it.slot },
        centerLabel = if (here == null) RadialActions.label(config.center) else "Back",
        centerHighlighted = here == null && centerHighlighted,
        origin = origin,
        onSlot = { slot -> entries.firstOrNull { it.slot.id == slot.id }?.onPick?.invoke() },
        onCenter = { if (here == null) onAction(config.center) else pop() },
        onDismiss = { if (here == null) onDismiss() else pop() },
    )
}

/**
 * One configured item as the ring entries it contributes: normally one slot, but an
 * `expand`ed dynamic node splices its whole source in (which is how a ring can be the
 * session list directly, the way the palette looked before submenus existed).
 */
private fun expand(
    index: Int,
    item: RadialItem,
    dynamic: (String) -> List<RadialDynamicEntry>,
    pick: (RadialItem) -> Unit,
): List<RadialDynamicEntry> = when {
    item is RadialItem.Dynamic && item.expand -> dynamic(item.source)
    // Labels can repeat, so a slot's id is its position in this ring — ids only have to
    // be unique within the ring that's on screen.
    else -> listOf(
        RadialDynamicEntry(PaletteSlot(id = "i$index", label = item.label)) { pick(item) },
    )
}

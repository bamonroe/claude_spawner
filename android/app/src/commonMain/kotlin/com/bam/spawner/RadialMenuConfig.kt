package com.bam.spawner

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.add
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.contentOrNull
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put

/**
 * The user-configurable shape of the radial menu (double-tap the chat log).
 *
 * A menu is a *tree*: every ring has a centre and up to [PALETTE_SLOTS] items, and an
 * item may itself be a [RadialItem.Submenu] that opens **in place** — same origin, same
 * geometry, only the ring's contents change (see [RadialMenuHost]). That's what lets a
 * top-level "Sessions" button lead to the session ring the palette used to show flat.
 *
 * Persisted as JSON in [Prefs.radialMenu] so Android and the web client share one
 * definition and one editor ([RadialMenuSettings]).
 */
sealed class RadialItem {
    abstract val label: String

    /** Runs a built-in action from [RadialActions] and closes the ring. */
    data class Action(override val label: String, val action: String) : RadialItem()

    /** Opens [items] in place, replacing the ring without moving it. */
    data class Submenu(override val label: String, val items: List<RadialItem> = emptyList()) : RadialItem()

    /**
     * A ring built at open time from live app state ([RadialSources]) rather than config.
     * With [expand] the source's entries are spliced straight into this ring; without it
     * this is one button that descends into them as a submenu.
     */
    data class Dynamic(
        override val label: String,
        val source: String,
        val expand: Boolean = false,
    ) : RadialItem()
}

/** One whole ring: what the centre button does, and what sits around it. */
data class RadialMenuConfig(
    /** Action id for the centre button — see [RadialActions]. */
    val center: String = RadialActions.MIC_LOCK,
    val items: List<RadialItem> = emptyList(),
)

/** The built-in actions a slot (or the centre) can be bound to. */
object RadialActions {
    const val MIC_LOCK = "mic_lock"
    const val NEW_SESSION = "new_session"
    const val SWAP = "swap"
    const val DETACH = "detach"
    const val HANDS_FREE = "hands_free"
    const val STOP_SPEAKING = "stop_speaking"
    const val USAGE = "usage"
    const val SETTINGS = "settings"
    const val CLOSE = "close"

    /** id → label shown in the editor's picker. Order is the picker's order. */
    val all: List<Pair<String, String>> = listOf(
        MIC_LOCK to "Lock mic",
        NEW_SESSION to "New session",
        SWAP to "Swap session",
        DETACH to "Detach",
        HANDS_FREE to "Hands-free",
        STOP_SPEAKING to "Stop speaking",
        USAGE to "Check usage",
        SETTINGS to "Settings",
        CLOSE to "Close menu",
    )

    fun label(id: String): String = all.firstOrNull { it.first == id }?.second ?: id
}

/** The live-state rings a [RadialItem.Dynamic] can draw from. */
object RadialSources {
    /** The attach-history ring: recent sessions, most recent first. */
    const val SESSIONS = "sessions"

    /**
     * The tray-command ring: the same curated, argument-free commands the swipe-up
     * tray shows (Settings › Commands), each sending its "hey buddy …" phrase.
     */
    const val COMMANDS = "commands"

    val all: List<Pair<String, String>> = listOf(
        SESSIONS to "Sessions",
        COMMANDS to "Commands",
    )

    fun label(id: String): String = all.firstOrNull { it.first == id }?.second ?: id
}

/**
 * The out-of-the-box menu: the centre still locks the mic (unchanged), and the sessions
 * ring the palette used to show flat is now the "Sessions" submenu, opened in place.
 */
val DefaultRadialMenu = RadialMenuConfig(
    center = RadialActions.MIC_LOCK,
    items = listOf(
        RadialItem.Dynamic("Sessions", RadialSources.SESSIONS),
        RadialItem.Dynamic("Commands", RadialSources.COMMANDS),
        RadialItem.Action("New", RadialActions.NEW_SESSION),
        RadialItem.Action("Swap", RadialActions.SWAP),
        RadialItem.Action("Hands-free", RadialActions.HANDS_FREE),
        RadialItem.Action("Settings", RadialActions.SETTINGS),
    ),
)

// JSON by hand on the JsonElement API, the same way the wire protocol is read and
// written here — this module carries no kotlinx-serialization compiler plugin.
private val radialJson = Json { ignoreUnknownKeys = true }

/** Parse a stored menu; blank or corrupt config falls back to [DefaultRadialMenu]. */
fun parseRadialMenu(json: String): RadialMenuConfig {
    if (json.isBlank()) return DefaultRadialMenu
    return runCatching {
        val o = radialJson.parseToJsonElement(json).jsonObject
        RadialMenuConfig(
            center = o["center"]?.jsonPrimitive?.contentOrNull ?: RadialActions.MIC_LOCK,
            items = readItems(o["items"]?.jsonArray),
        )
    }.getOrDefault(DefaultRadialMenu)
}

private fun readItems(arr: JsonArray?): List<RadialItem> =
    arr.orEmpty().mapNotNull { readItem(it.jsonObject) }

private fun readItem(o: JsonObject): RadialItem? {
    val label = o["label"]?.jsonPrimitive?.contentOrNull ?: ""
    return when (o["type"]?.jsonPrimitive?.contentOrNull) {
        "action" -> RadialItem.Action(label, o["action"]?.jsonPrimitive?.contentOrNull ?: return null)
        "submenu" -> RadialItem.Submenu(label, readItems(o["items"]?.jsonArray))
        "dynamic" -> RadialItem.Dynamic(
            label,
            o["source"]?.jsonPrimitive?.contentOrNull ?: return null,
            o["expand"]?.jsonPrimitive?.boolean ?: false,
        )
        else -> null
    }
}

fun encodeRadialMenu(config: RadialMenuConfig): String = buildJsonObject {
    put("center", config.center)
    put("items", writeItems(config.items))
}.toString()

private fun writeItems(items: List<RadialItem>): JsonArray = buildJsonArray {
    items.forEach { item ->
        add(
            buildJsonObject {
                put("label", item.label)
                when (item) {
                    is RadialItem.Action -> { put("type", "action"); put("action", item.action) }
                    is RadialItem.Submenu -> { put("type", "submenu"); put("items", writeItems(item.items)) }
                    is RadialItem.Dynamic -> {
                        put("type", "dynamic"); put("source", item.source); put("expand", item.expand)
                    }
                }
            },
        )
    }
}

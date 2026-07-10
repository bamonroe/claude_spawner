package com.bam.spawner

import java.io.File
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonArray
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * End-to-end drift check for the command pipeline's client half: the compiled-in
 * [COMMANDS] list (emitted by the generateCommands Gradle task) must match
 * docs/commands.json exactly — every command, alias, and description. The server
 * half (registry -> commands.json) is covered by internal/command's Go tests;
 * this closes the gap where the Gradle generator itself could silently drop or
 * mangle an entry (e.g. an escaping bug). Runs with `:app:testDebugUnitTest`.
 */
class CommandsSyncTest {

    // Unit tests run with the module dir (android/app) as the working directory;
    // the shared JSON lives at the repo root.
    private val jsonFile = File("../../docs/commands.json")

    @Test
    fun compiledCommandsMatchJson() {
        assertTrue(jsonFile.isFile, "docs/commands.json not found at ${jsonFile.absolutePath}")
        val fromJson = Json.parseToJsonElement(jsonFile.readText())
            .jsonObject.getValue("commands").jsonArray
            .map { el ->
                val o = el.jsonObject
                Command(
                    name = o.getValue("title").jsonPrimitive.content,
                    aliases = o.getValue("aliases").jsonArray.map { it.jsonPrimitive.content },
                    description = o.getValue("description").jsonPrimitive.content,
                )
            }
        assertTrue(fromJson.isNotEmpty(), "commands.json parsed to an empty command list")
        assertEquals(
            fromJson.sortedBy { it.name }, COMMANDS,
            "compiled COMMANDS drifted from docs/commands.json — " +
                "the generateCommands Gradle task mistranslated it",
        )
    }
}

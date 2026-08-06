package com.bam.spawner

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * `chatKeys` feeds LazyColumn's item keys, where a duplicate key is a hard crash
 * and an unstable key throws away every composed bubble. So the two properties
 * that matter are: unique across the whole list, and unchanged for a row that
 * hasn't changed identity.
 */
class ChatKeysTest {
    private fun msg(
        id: String = "",
        liveKey: String = "",
        index: Int = -1,
    ) = ChatMessage(role = Role.CLAUDE, text = "t", index = index, id = id, liveKey = liveKey)

    @Test
    fun prefersBackendIdThenLiveKeyThenIndexThenPosition() {
        val keys = chatKeys(
            listOf(
                msg(id = "a", liveKey = "9:1", index = 3),
                msg(liveKey = "9:2", index = 4),
                msg(index = 5),
                msg(),
            ),
        )
        assertEquals(listOf("i:a", "l:9:2", "x:5", "p:3"), keys)
    }

    @Test
    fun repeatedBaseKeysAreMadeUnique() {
        val keys = chatKeys(listOf(msg(id = "a"), msg(id = "a"), msg(id = "a"), msg(id = "b")))
        assertEquals(keys.size, keys.toSet().size)
        assertEquals(listOf("i:a", "i:a#1", "i:a#2", "i:b"), keys)
    }

    @Test
    fun keysAreStableWhenNewerMessagesAreAppended() {
        val before = listOf(msg(id = "a"), msg(liveKey = "9:1"))
        val after = before + msg(liveKey = "9:2")
        assertEquals(chatKeys(before), chatKeys(after).take(before.size))
    }

    @Test
    fun emptyListYieldsNoKeys() {
        assertTrue(chatKeys(emptyList()).isEmpty())
    }
}

package com.bam.spawner

import com.bam.spawner.net.normalizeWsUrl
import kotlin.test.Test
import kotlin.test.assertEquals

/**
 * Locks the "type a bare host" behaviour of [normalizeWsUrl]: a memorable host
 * like `cs.bam` must resolve to the full gateway URL, while an already-complete
 * URL (any scheme/path the user typed) is left alone.
 */
class NormalizeWsUrlTest {
    @Test fun bareHostGetsSchemeAndPath() {
        assertEquals("ws://cs.bam/ws", normalizeWsUrl("cs.bam"))
    }

    @Test fun bareHostWithPortGetsSchemeAndPath() {
        assertEquals("ws://cs.bam:8098/ws", normalizeWsUrl("cs.bam:8098"))
    }

    @Test fun httpSchemeMapsToWs() {
        assertEquals("ws://cs.bam/ws", normalizeWsUrl("http://cs.bam"))
    }

    @Test fun httpsSchemeMapsToWss() {
        assertEquals("wss://cs.bam/ws", normalizeWsUrl("https://cs.bam"))
    }

    @Test fun barePathSlashBecomesWs() {
        assertEquals("ws://cs.bam/ws", normalizeWsUrl("ws://cs.bam/"))
    }

    @Test fun explicitWsUrlIsUntouched() {
        assertEquals("ws://100.64.0.2:8098/ws", normalizeWsUrl("ws://100.64.0.2:8098/ws"))
    }

    @Test fun explicitCustomPathIsKept() {
        assertEquals("wss://host/custom/path", normalizeWsUrl("wss://host/custom/path"))
    }

    @Test fun surroundingWhitespaceIsTrimmed() {
        assertEquals("ws://cs.bam/ws", normalizeWsUrl("  cs.bam  "))
    }

    @Test fun blankStaysBlank() {
        assertEquals("", normalizeWsUrl("   "))
    }
}

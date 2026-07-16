package com.bam.spawner

import com.bam.spawner.net.CatalogueDigest
import com.bam.spawner.net.SettingRecord
import kotlin.test.Test
import kotlin.test.assertEquals

/**
 * Cross-language parity for the fifth (settings) catalogue's digest. The
 * skip-if-equal fast path only works if the Kotlin fold computes the byte-identical
 * value the Go server does — so this pins the SAME fixture and known hex the Go test
 * asserts (server/internal/gateway/catalogdigest_test.go: TestSettingsDigestFold).
 * If either side's canonical record scheme drifts, one of the two tests goes red.
 */
class CatalogueDigestTest {

    @Test
    fun settingsFoldMatchesGoKnownHex() {
        val recs = listOf(
            SettingRecord("summary_only", "true", 100),
            SettingRecord("auto_compress", "false", 200),
        )
        assertEquals("d7a850f0b07c87bd", CatalogueDigest.settings(recs))
        // Order-independent (wrapping sum of per-record FNV-1a).
        assertEquals("d7a850f0b07c87bd", CatalogueDigest.settings(recs.reversed()))
        // Empty folds to all-zero.
        assertEquals("0000000000000000", CatalogueDigest.settings(emptyList()))
    }
}

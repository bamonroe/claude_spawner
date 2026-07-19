package com.bam.spawner

import com.bam.spawner.net.CatalogueDigest
import com.bam.spawner.net.SettingRecord
import com.bam.spawner.net.SpokenTokenInfo
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

    /**
     * Cross-language parity for the spoken-token catalogue's digest. Pins the SAME
     * fixture and known hex the Go test asserts (TestSpokenTokensDigestFold).
     */
    @Test
    fun spokenTokensFoldMatchesGoKnownHex() {
        val toks = listOf(
            SpokenTokenInfo("hey-buddy", "hey buddy", "wake", "bump_bump", 100),
            SpokenTokenInfo("end-token", "beep", "end", "", 200),
        )
        assertEquals("0f155d6e0bcbfc37", CatalogueDigest.spokenTokens(toks))
        assertEquals("0f155d6e0bcbfc37", CatalogueDigest.spokenTokens(toks.reversed()))
        assertEquals("0000000000000000", CatalogueDigest.spokenTokens(emptyList()))
    }
}

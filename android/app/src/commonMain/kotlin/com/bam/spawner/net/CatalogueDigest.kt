package com.bam.spawner.net

/**
 * The app side of the per-catalogue **skip-if-equal** fast path. Each app-managed
 * catalogue (hosts, identities, profiles, providers) folds its live records'
 * `(key, updated_at, payload)` into one stable, order-INDEPENDENT checksum; the app
 * presents all four in the `hello` handshake and the server re-sends only the
 * catalogues whose digest differs — mirroring the chat transcript's count+hash
 * `digests`/`history unchanged` shortcut, generalized to the catalogues.
 *
 * This MUST compute the identical value the server does in
 * `server/internal/gateway/catalogdigest.go`. To keep that parity trivial across
 * Android + wasmJs `commonMain` with no shared crypto dependency, the fold is a
 * 64-bit FNV-1a per record, wrapping-summed across records and hex-encoded (not a
 * SHA — this is a cache-validation checksum, only ever equality-compared). The
 * empty set folds to all-zero.
 *
 * The identity fold excludes the password (server-only; the app only knows
 * `hasPassword`); a password edit still flips the digest via `updatedAt`. Field
 * order, list order, and sorted map-key order all match the Go side exactly.
 */
object CatalogueDigest {
    private const val FS = "" // between a record's fields (US)
    private const val RS = "" // between list/map elements within one field (RS)

    private const val FNV_OFFSET = 0xcbf29ce484222325uL
    private const val FNV_PRIME = 0x00000100000001b3uL

    private fun fnv1a(s: String): ULong {
        var h = FNV_OFFSET
        for (b in s.encodeToByteArray()) {
            h = h xor b.toUByte().toULong()
            h *= FNV_PRIME
        }
        return h
    }

    private fun fold(records: List<String>): String {
        var sum = 0uL
        for (r in records) sum += fnv1a(r)
        return sum.toString(16).padStart(16, '0')
    }

    private fun list(xs: List<String>) = xs.joinToString(RS)
    private fun map(m: Map<String, String>) =
        m.entries.sortedBy { it.key }.joinToString(RS) { "${it.key}=${it.value}" }

    fun hosts(hs: List<Host>): String = fold(hs.map {
        listOf(
            it.name, it.address, it.user, it.port.toString(),
            it.keyFile, it.identity, it.claudeBin, it.updatedAt.toString(),
        ).joinToString(FS)
    })

    fun identities(ids: List<Identity>): String = fold(ids.map {
        listOf(it.name, it.user, it.publicKey, it.updatedAt.toString()).joinToString(FS)
    })

    fun profiles(ps: List<ProfileInfo>): String = fold(ps.map {
        listOf(
            it.name, it.target, it.default.toString(), it.image, it.homeMount,
            list(it.mounts), list(it.creds), map(it.env), list(it.runArgs),
            map(it.vars), it.updatedAt.toString(),
        ).joinToString(FS)
    })

    fun providers(ag: List<AgentInfo>): String = fold(ag.map {
        listOf(
            it.id, it.name, it.defaultModel,
            list(it.models), list(it.voiceModels), it.updatedAt.toString(),
        ).joinToString(FS)
    })

    // The fifth catalogue: keyed shared-settings records, folded (key, value,
    // updated_at) with the same scheme — byte-identical to Go's settingsDigest.
    fun settings(ss: List<SettingRecord>): String = fold(ss.map {
        listOf(it.key, it.value, it.updatedAt.toString()).joinToString(FS)
    })
}

/** The four catalogue digests the app presents in the `hello` handshake so the
 *  server can skip re-sending an unchanged catalogue on connect. Empty strings
 *  (an older client's default) mismatch every server digest, so it re-sends all. */
data class CatalogueDigests(
    val hosts: String = "",
    val identities: String = "",
    val profiles: String = "",
    val providers: String = "",
    val settings: String = "",
)

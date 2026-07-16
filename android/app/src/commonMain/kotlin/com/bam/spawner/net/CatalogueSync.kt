package com.bam.spawner.net

import com.bam.spawner.nowEpochMs
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * One app-managed catalogue's reconciliation, declared once. Holds the `StateFlow`
 * the UI reads and folds an inbound server list into it by **last-writer-wins on
 * [updatedAt]** (Phase 2a): the server's broadcast is authoritative for membership
 * (a key it omits was deleted/tombstoned, so it drops from the local list), but a
 * local record whose [updatedAt] is *strictly newer* than the incoming one for the
 * same [key] is preserved — guarding a just-made local edit from being clobbered by
 * a broadcast triggered by a different client's change that raced ahead of ours. On
 * an equal stamp the incoming record wins (so e.g. a `set_default` marker flip, which
 * doesn't bump `updated_at`, still applies). The server holds the max and rejects
 * stale writes, so in the steady state incoming already carries the newest value.
 */
class Catalogue<T>(
    private val key: (T) -> String,
    private val updatedAt: (T) -> Long,
) {
    private val _items = MutableStateFlow<List<T>>(emptyList())
    val items: StateFlow<List<T>> = _items.asStateFlow()

    /** The record identity function (stable name/id). */
    fun keyOf(record: T): String = key(record)

    /** Fold an inbound server list into the current state, last-writer-wins per key. */
    fun apply(incoming: List<T>) {
        val localByKey = _items.value.associateBy(key)
        _items.value = incoming.map { inc ->
            val loc = localByKey[key(inc)]
            if (loc != null && updatedAt(loc) > updatedAt(inc)) loc else inc
        }
    }
}

/**
 * The single, shared reconciliation point for the four app-managed catalogues —
 * hosts, identities, execution profiles, and provider (AI-backend) overlays. Both
 * clients ([com.bam.spawner.VoiceController] on Android, the web controller on wasmJs)
 * own one of these and route their inbound catalogue apply and outbound catalogue
 * mutators through it, so the reconcile logic lives in `commonMain` exactly once and
 * the two controllers can't drift.
 *
 * The app is the source of truth on first write; the server persists each catalogue and
 * re-broadcasts the corresponding list message, which [apply] folds back in. This slice
 * is a pure refactor — no wire change, no timestamps — but the [Catalogue] merge seam is
 * where the versioned (`updated_at` last-writer-wins + tombstones + per-catalogue digest)
 * conflict model lands next, in one place for both clients.
 *
 * [send] is the platform's socket writer (`client?.send(...)`); a null/closed client
 * simply drops the frame, matching the prior `client?.send(...)` behavior.
 */
class CatalogueSync(private val send: (String) -> Unit) {
    private val hostCat = Catalogue<Host>(key = Host::name, updatedAt = Host::updatedAt)
    private val identityCat = Catalogue<Identity>(key = Identity::name, updatedAt = Identity::updatedAt)
    private val profileCat = Catalogue<ProfileInfo>(key = ProfileInfo::name, updatedAt = ProfileInfo::updatedAt)
    private val agentCat = Catalogue<AgentInfo>(key = AgentInfo::id, updatedAt = AgentInfo::updatedAt)
    private val settingCat = Catalogue<SettingRecord>(key = SettingRecord::key, updatedAt = SettingRecord::updatedAt)

    val hosts: StateFlow<List<Host>> = hostCat.items
    val identities: StateFlow<List<Identity>> = identityCat.items
    val profiles: StateFlow<List<ProfileInfo>> = profileCat.items
    val agents: StateFlow<List<AgentInfo>> = agentCat.items
    /** The shared server-global settings catalogue, as keyed records. Controllers
     *  read typed values off this (see [settingValue]) and mirror them into the UI. */
    val settings: StateFlow<List<SettingRecord>> = settingCat.items

    /**
     * The four catalogues' per-record digests for the `hello` handshake: the server
     * skips re-sending any catalogue whose digest we already match (see
     * [CatalogueDigest]). Computed from the currently-held records, so a reconnect
     * presents whatever the app has cached this session (catalogues aren't persisted
     * across process restarts, so a fresh start yields empty digests → the server
     * simply falls back to broadcasting — safe).
     */
    fun digests() = CatalogueDigests(
        hosts = CatalogueDigest.hosts(hostCat.items.value),
        identities = CatalogueDigest.identities(identityCat.items.value),
        profiles = CatalogueDigest.profiles(profileCat.items.value),
        providers = CatalogueDigest.providers(agentCat.items.value),
        settings = CatalogueDigest.settings(settingCat.items.value),
    )

    /** The current string value of a shared setting, or null when the server hasn't
     *  advertised it yet — the caller types it and falls back to its own default. */
    fun settingValue(key: String): String? = settingCat.items.value.firstOrNull { it.key == key }?.value

    /**
     * Reconcile an inbound server message if it is one of the four catalogue lists.
     * Returns true when [msg] was handled (a catalogue list), false otherwise so the
     * caller's `when` can fall through to the session/chat branches it still owns.
     */
    fun apply(msg: ServerMsg): Boolean = when (msg) {
        is ServerMsg.HostList -> { hostCat.apply(msg.hosts); true }
        is ServerMsg.IdentityList -> { identityCat.apply(msg.identities); true }
        is ServerMsg.Profiles -> { profileCat.apply(msg.profiles); true }
        is ServerMsg.Agents -> { agentCat.apply(msg.agents); true }
        is ServerMsg.Settings -> { settingCat.apply(msg.settings); true }
        else -> false
    }

    // Every local edit is stamped with the wall-clock ms at push time; the server's
    // last-writer-wins arbitration keeps the newest and rejects/echoes older writes.
    // --- Hosts (Settings → Hosts) --------------------------------------------
    fun requestHosts() = send(Outbound.hostsList())
    fun putHost(host: Host) = send(Outbound.hostPut(host.copy(updatedAt = nowEpochMs())))
    fun deleteHost(name: String) = send(Outbound.hostDelete(name, nowEpochMs()))

    // --- Identities (Settings → Identities) ----------------------------------
    fun requestIdentities() = send(Outbound.identitiesList())
    fun createIdentity(name: String, user: String, password: String, genKey: Boolean) =
        send(Outbound.identityCreate(name, user, password, genKey, nowEpochMs()))
    fun importIdentity(name: String, user: String, password: String, keyPath: String) =
        send(Outbound.identityImport(name, user, password, keyPath, nowEpochMs()))
    fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) =
        send(Outbound.identityUpdate(name, user, setPassword, password, nowEpochMs()))
    fun deleteIdentity(name: String) = send(Outbound.identityDelete(name, nowEpochMs()))

    // --- Execution profiles (Settings → Profiles) ----------------------------
    fun putProfile(p: ProfileInfo) = send(Outbound.profilePut(p.copy(updatedAt = nowEpochMs())))
    fun deleteProfile(name: String) = send(Outbound.profileDelete(name, nowEpochMs()))
    fun setDefaultProfile(name: String) = send(Outbound.profileSetDefault(name))

    // --- Providers / AI-backend overlays (Settings → Providers) --------------
    fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>) =
        send(Outbound.providerPut(agent, defaultModel, voiceModels, nowEpochMs()))

    // --- Shared settings (whisper models, auto-compress, summary-only) --------
    // Each scalar is its own keyed record so per-key last-writer-wins arbitrates
    // independent toggles. Value is always a string (bool/int stringified by the caller).
    fun putSetting(key: String, value: String) = send(Outbound.settingPut(key, value, nowEpochMs()))
}

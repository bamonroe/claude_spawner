package com.bam.spawner.net

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * One app-managed catalogue's reconciliation, declared once. Holds the `StateFlow`
 * the UI reads and knows how to fold an inbound server list into it. [key] identifies
 * a record (its stable name/id) and [merge] is the *merge rule* — the seam the unified
 * sync layer grows into: today it is a whole-list replace (blind last-write-wins, which
 * is exactly what both controllers did inline), but Phase 2 will swap in a per-record
 * `updated_at` last-writer-wins merge (keyed by [key]) without touching any call site.
 */
class Catalogue<T>(
    private val key: (T) -> String,
    private val merge: (current: List<T>, incoming: List<T>) -> List<T> = { _, incoming -> incoming },
) {
    private val _items = MutableStateFlow<List<T>>(emptyList())
    val items: StateFlow<List<T>> = _items.asStateFlow()

    /** The record identity function — the merge seam's key (unused until Phase 2's LWW merge). */
    fun keyOf(record: T): String = key(record)

    /** Fold an inbound server list into the current state via the [merge] rule. */
    fun apply(incoming: List<T>) { _items.value = merge(_items.value, incoming) }
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
    private val hostCat = Catalogue<Host>(key = Host::name)
    private val identityCat = Catalogue<Identity>(key = Identity::name)
    private val profileCat = Catalogue<ProfileInfo>(key = ProfileInfo::name)
    private val agentCat = Catalogue<AgentInfo>(key = AgentInfo::id)

    val hosts: StateFlow<List<Host>> = hostCat.items
    val identities: StateFlow<List<Identity>> = identityCat.items
    val profiles: StateFlow<List<ProfileInfo>> = profileCat.items
    val agents: StateFlow<List<AgentInfo>> = agentCat.items

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
        else -> false
    }

    // --- Hosts (Settings → Hosts) --------------------------------------------
    fun requestHosts() = send(Outbound.hostsList())
    fun putHost(host: Host) = send(Outbound.hostPut(host))
    fun deleteHost(name: String) = send(Outbound.hostDelete(name))

    // --- Identities (Settings → Identities) ----------------------------------
    fun requestIdentities() = send(Outbound.identitiesList())
    fun createIdentity(name: String, user: String, password: String, genKey: Boolean) =
        send(Outbound.identityCreate(name, user, password, genKey))
    fun importIdentity(name: String, user: String, password: String, keyPath: String) =
        send(Outbound.identityImport(name, user, password, keyPath))
    fun updateIdentity(name: String, user: String, setPassword: Boolean, password: String) =
        send(Outbound.identityUpdate(name, user, setPassword, password))
    fun deleteIdentity(name: String) = send(Outbound.identityDelete(name))

    // --- Execution profiles (Settings → Profiles) ----------------------------
    fun putProfile(p: ProfileInfo) = send(Outbound.profilePut(p))
    fun deleteProfile(name: String) = send(Outbound.profileDelete(name))
    fun setDefaultProfile(name: String) = send(Outbound.profileSetDefault(name))

    // --- Providers / AI-backend overlays (Settings → Providers) --------------
    fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>) =
        send(Outbound.providerPut(agent, defaultModel, voiceModels))
}

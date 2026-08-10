package com.bam.spawner.net

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * Everything the UI knows about one host's Claude login: who it is logged in as
 * (from `auth_status`), plus whatever login attempt is currently in flight.
 *
 * A login belongs to the *host*, not to the connection that started it — the server
 * broadcasts these frames — so state is keyed by host name and any client watching
 * sees the same thing.
 */
data class AuthState(
    val loggedIn: Boolean = false,
    val authMethod: String = "",
    val email: String = "",
    val orgName: String = "",
    val subscriptionType: String = "",
    /** The server's human-readable one-liner, ready to show or speak. */
    val text: String = "",
    /** True between [AuthSync.startLogin] and the terminal `auth_login_result`. */
    val loginInFlight: Boolean = false,
    /** The browser URL from `auth_login_url`; empty when there's none to open. */
    val loginUrl: String = "",
    /** The login method actually used ("" = the host's current billing identity). */
    val loginMethod: String = "",
    /** True once a URL has arrived and the server is waiting on the pasted code. */
    val awaitingCode: Boolean = false,
    /** The last failed login's error; cleared when a new attempt starts. */
    val lastError: String = "",
    /**
     * A turn on this host just failed on a credential problem (`turn_failed_auth`), so
     * the UI should offer a re-login without waiting to be asked. Cleared by the next
     * `auth_status` that says we're logged in, and by starting a login.
     */
    val needsLogin: Boolean = false,
)

/**
 * The single, shared reconciliation point for the Claude re-login (`claude auth`)
 * flow — the sibling of [CatalogueSync] for auth rather than catalogues. Both clients
 * ([com.bam.spawner.VoiceController] on Android, the web controller on wasmJs) own one
 * and route the three inbound auth frames and the five outbound auth messages through
 * it, so the state machine lives in `commonMain` exactly once and can't drift.
 *
 * The flow it models: [requestAuthStatus] to see who's logged in, [startLogin] to begin
 * (method "" reuses the host's current billing identity), open the URL that arrives as
 * `auth_login_url`, then [submitCode] with what the browser hands the user — the verdict
 * arrives as `auth_login_result`.
 *
 * [send] is the platform's socket writer (`client?.send(...)`); a null/closed client
 * simply drops the frame.
 */
class AuthSync(private val send: (String) -> Unit) {
    private val _states = MutableStateFlow<Map<String, AuthState>>(emptyMap())

    /** Per-host auth state, keyed by host name ("" = the configured target host). */
    val states: StateFlow<Map<String, AuthState>> = _states.asStateFlow()

    /** The state for [host], defaulted so callers never deal with a null. */
    fun state(host: String = ""): AuthState = _states.value[host] ?: AuthState()

    /**
     * Fold an inbound server message in if it is one of the three auth frames. Returns
     * true when [msg] was handled, false otherwise so the caller's `when` can fall
     * through to the branches it still owns — same contract as [CatalogueSync.apply].
     */
    fun apply(msg: ServerMsg): Boolean = when (msg) {
        is ServerMsg.AuthStatus -> {
            update(msg.host) {
                it.copy(
                    loggedIn = msg.loggedIn, authMethod = msg.authMethod, email = msg.email,
                    orgName = msg.orgName, subscriptionType = msg.subscriptionType, text = msg.text,
                    needsLogin = it.needsLogin && !msg.loggedIn,
                )
            }
            true
        }
        // A URL means the server is now parked waiting on the code the browser shows.
        is ServerMsg.AuthLoginUrl -> {
            update(msg.host) {
                it.copy(loginInFlight = true, loginUrl = msg.url, loginMethod = msg.method, awaitingCode = true)
            }
            true
        }
        // Terminal either way: the attempt is over, so the in-flight bits all clear.
        is ServerMsg.AuthLoginResult -> {
            update(msg.host) {
                it.copy(
                    loginInFlight = false, loginUrl = "", awaitingCode = false,
                    lastError = if (msg.ok) "" else msg.error,
                )
            }
            // The server re-broadcasts `auth_status` after a successful login, so we
            // don't guess at the new identity here — we wait to be told.
            true
        }
        else -> false
    }

    // --- Outbound (Settings → Hosts, and the inline re-login prompt) -------------
    fun requestAuthStatus(host: String = "") = send(Outbound.authStatus(host))

    fun startLogin(host: String = "", method: String = "") {
        update(host) {
            it.copy(
                loginInFlight = true, loginUrl = "", loginMethod = method,
                awaitingCode = false, lastError = "", needsLogin = false,
            )
        }
        send(Outbound.authLogin(host, method))
    }

    fun submitCode(code: String, host: String = "") {
        update(host) { it.copy(awaitingCode = false) }
        send(Outbound.authLoginCode(code, host))
    }

    fun cancelLogin(host: String = "") {
        update(host) { it.copy(loginInFlight = false, loginUrl = "", awaitingCode = false) }
        send(Outbound.authLoginCancel(host))
    }

    fun logout(host: String = "") = send(Outbound.authLogout(host))

    /**
     * Record that a turn on [host] died on expired/missing credentials, and re-check the
     * status so the offer the UI makes is based on the server's answer rather than our
     * inference. The router calls this on `turn_failed_auth`.
     */
    fun noteAuthFailure(host: String = "") {
        update(host) { it.copy(needsLogin = true) }
        requestAuthStatus(host)
    }

    private fun update(host: String, f: (AuthState) -> AuthState) {
        _states.value = _states.value + (host to f(_states.value[host] ?: AuthState()))
    }
}

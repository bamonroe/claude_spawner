package com.bam.spawner

import com.bam.spawner.net.AuthState
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice of the app controller the shared re-login UI needs — the sibling of
 * [HostsIdentitiesController] for Claude logins. Both controllers implement it by
 * delegating to the shared [com.bam.spawner.net.AuthSync], so the Hosts settings
 * screen and the inline "your credentials expired" prompt render identically on the
 * app and the web client.
 *
 * Every method takes a host name; "" means the configured target host.
 */
interface AuthController {
    /** Per-host auth state, keyed by host name. See [AuthState]. */
    val authStates: StateFlow<Map<String, AuthState>>
    fun requestAuthStatus(host: String = "")
    /** Start a login; [method] "" reuses the host's current billing identity. */
    fun startLogin(host: String = "", method: String = "")
    fun submitLoginCode(code: String, host: String = "")
    fun cancelLogin(host: String = "")
    fun logout(host: String = "")
}

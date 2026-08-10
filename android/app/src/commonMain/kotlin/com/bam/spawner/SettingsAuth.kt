package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.rememberUpdatedState
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.AuthState
import com.bam.spawner.ui.rememberLinkOpener
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

/** A settings sub-screen wrapper around [ClaudeLoginPanel] for one host. */
@Composable
fun ClaudeLoginSettings(controller: AuthController, host: String = "", onBack: () -> Unit) {
    SettingsScaffold(if (host.isEmpty()) "Claude login" else "Claude login — $host", onBack) {
        ClaudeLoginPanel(controller, host)
    }
}

/**
 * The whole `claude auth` re-login flow as one self-contained panel, in commonMain so
 * the app and the browser client render it identically: current identity, a billing
 * chooser, the OAuth URL step (open in a browser, or copy it to another device), the
 * pasted-code step, and the verdict.
 *
 * It drives [AuthController] only — every bit of state comes back from the server via
 * [AuthState], so two clients watching the same host stay in step. Navigating away
 * while an attempt is pending cancels it, since the URL dies with the login process.
 */
@Composable
fun ClaudeLoginPanel(controller: AuthController, host: String = "") {
    val states by controller.authStates.collectAsState()
    val auth = states[host] ?: AuthState()
    val opener = rememberLinkOpener()

    var method by rememberSaveable { mutableStateOf("") } // "" = keep the host's current identity
    var code by rememberSaveable { mutableStateOf("") }
    var copied by rememberSaveable { mutableStateOf(false) }
    var openFailed by rememberSaveable { mutableStateOf(false) }
    // Was a login pending on the previous frame? Lets us show a terminal verdict once
    // the in-flight flag clears, rather than silently snapping back to the status line.
    // Set when this panel starts an attempt, so the verdict below only shows for a
    // login the user actually watched — not for stale state from another client.
    var attempted by rememberSaveable { mutableStateOf(false) }
    val finished = attempted && !auth.loginInFlight

    // Ask once on entry, and cancel any attempt we leave behind.
    val live = rememberUpdatedState(auth.loginInFlight)
    DisposableEffect(host) {
        controller.requestAuthStatus(host)
        onDispose { if (live.value) controller.cancelLogin(host) }
    }

    Text("Claude login", style = MaterialTheme.typography.titleMedium)
    Text(
        if (auth.text.isNotEmpty()) auth.text
        else if (auth.loggedIn) buildString {
            append("Logged in")
            if (auth.email.isNotEmpty()) append(" as ${auth.email}")
            if (auth.subscriptionType.isNotEmpty()) append(" (${auth.subscriptionType})")
            if (auth.orgName.isNotEmpty()) append(" — ${auth.orgName}")
        }
        else "Logged out.",
        style = MaterialTheme.typography.bodyMedium,
        color = if (auth.loggedIn) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.error,
    )

    if (!auth.loginInFlight) {
        HorizontalDivider()
        Text("Bill this login to", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            FilterChip(method == "", { method = "" }, { Text("Current") })
            FilterChip(method == "claudeai", { method = "claudeai" }, { Text("Subscription") })
            FilterChip(method == "console", { method = "console" }, { Text("API console") })
        }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            Button(onClick = {
                code = ""; copied = false; openFailed = false; attempted = true
                controller.startLogin(host, method)
            }) { Text(if (auth.loggedIn) "Log in again" else "Log in") }
            if (auth.loggedIn) {
                OutlinedButton(onClick = { controller.logout(host) }) { Text("Log out") }
            }
        }
    }

    if (auth.loginInFlight) {
        HorizontalDivider()
        Surface(
            color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
            shape = RoundedCornerShape(12.dp),
            modifier = Modifier.fillMaxWidth(),
        ) {
            Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                if (auth.loginUrl.isEmpty()) {
                    Row(horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                        CircularProgressIndicator(Modifier.padding(2.dp))
                        Text("Starting login…", style = MaterialTheme.typography.bodyMedium)
                    }
                } else {
                    Text(
                        "Open this link, approve the login, then paste the code Anthropic shows you. "
                            + "The link works from any device.",
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                    )
                    Text(auth.loginUrl, style = MaterialTheme.typography.bodySmall)
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                        Button(onClick = { openFailed = !opener.open(auth.loginUrl) }) { Text("Open link") }
                        OutlinedButton(onClick = { copied = opener.copy(auth.loginUrl) }) {
                            Text(if (copied) "Copied" else "Copy link")
                        }
                    }
                    if (openFailed) {
                        Text(
                            "No browser could open that — copy the link to another device instead.",
                            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error,
                        )
                    }
                    OutlinedTextField(
                        value = code, onValueChange = { code = it },
                        label = { Text("Code from the browser") },
                        singleLine = true, modifier = Modifier.fillMaxWidth(),
                    )
                    Button(
                        onClick = { controller.submitLoginCode(code.trim(), host); code = "" },
                        enabled = code.isNotBlank(),
                    ) { Text("Submit code") }
                    if (!auth.awaitingCode) {
                        Row(horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                            CircularProgressIndicator(Modifier.padding(2.dp))
                            Text("Finishing login…", style = MaterialTheme.typography.bodyMedium)
                        }
                    }
                }
                TextButton(onClick = { controller.cancelLogin(host) }) { Text("Cancel") }
            }
        }
    } else if (finished) {
        Text(
            if (auth.lastError.isEmpty()) "Login succeeded." else "Login failed: ${auth.lastError}",
            style = MaterialTheme.typography.bodyMedium,
            color = if (auth.lastError.isEmpty()) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.error,
        )
    }
}

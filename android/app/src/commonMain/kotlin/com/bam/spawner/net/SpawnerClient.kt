package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.plugins.websocket.webSocket
import io.ktor.websocket.Frame
import io.ktor.websocket.close
import io.ktor.websocket.readText
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

/**
 * Turn whatever the user typed in the "Server URL" field into a full WebSocket URL.
 *
 * The goal is that a memorable bare host — e.g. `cs.bam` — just works: the scheme
 * (`ws://`) and the gateway path (`/ws`) are filled in for them. So `cs.bam`,
 * `cs.bam:8098`, `http://cs.bam`, and `ws://cs.bam/ws` all resolve to the same
 * endpoint. Rules:
 *  - no `scheme://` → prepend `ws://`;
 *  - `http`/`https` schemes are mapped to `ws`/`wss` (a pasted browser URL still works);
 *  - `ws`/`wss` (or anything else) are kept as typed;
 *  - an empty or bare `/` path → `/ws`; an explicit path is left untouched.
 * A blank field is returned unchanged (the caller decides what to do with it).
 */
fun normalizeWsUrl(raw: String): String {
    val trimmed = raw.trim()
    if (trimmed.isEmpty()) return trimmed

    val sep = trimmed.indexOf("://")
    var s = if (sep >= 0) {
        val ws = when (trimmed.substring(0, sep).lowercase()) {
            "http" -> "ws"
            "https" -> "wss"
            else -> trimmed.substring(0, sep) // ws, wss, or leave as typed
        }
        "$ws://${trimmed.substring(sep + 3)}"
    } else {
        "ws://$trimmed"
    }

    // Ensure a gateway path. Look past the scheme for the authority's first '/'.
    val authStart = s.indexOf("://") + 3
    val pathStart = s.indexOf('/', authStart)
    s = when {
        pathStart < 0 -> "$s/ws"                       // host only, no path
        s.substring(pathStart) == "/" -> s.substring(0, pathStart) + "/ws"
        else -> s                                       // explicit path, keep it
    }
    return s
}

/**
 * WebSocket client to the spawner gateway, shared by the Android and web clients.
 * Sends the hello handshake (with a stable client_id for resume) on open, surfaces
 * parsed [ServerMsg]s, and automatically reconnects with backoff until [close] is
 * called. Audio is sent as binary frames; everything else is JSON text.
 *
 * The transport is Ktor: the platform [spawnerHttpClient] provides the engine
 * (OkHttp on Android, the browser WebSocket on wasmJs). TLS, when used, is
 * terminated at the reverse proxy — the app just connects with `wss://` + token.
 */
class SpawnerClient(
    private val url: String,
    private val token: String,
    private val clientId: String,
    private val hello: HelloConfig,
    private val onMessage: (ServerMsg) -> Unit,
    private val onConnected: (Boolean) -> Unit,
) {
    private val client: HttpClient = spawnerHttpClient()
    private val scope = CoroutineScope(Dispatchers.Default + SupervisorJob())

    // Outgoing frames are funnelled through one channel so sends from any thread
    // keep their order and never touch the session concurrently. trySend drops
    // when the buffer is full or we're between connections (matches the old
    // "drop when the socket is down" behaviour).
    private val outbox = Channel<Frame>(capacity = 256)

    private var active = false

    fun connect() {
        active = true
        scope.launch { runLoop() }
    }

    private suspend fun runLoop() {
        var attempt = 0
        while (active && scope.isActive) {
            try {
                client.webSocket(urlString = normalizeWsUrl(url)) {
                    attempt = 0
                    send(Frame.Text(Outbound.hello(token, clientId, hello)))
                    val sender = launch { for (frame in outbox) send(frame) }
                    try {
                        for (frame in incoming) {
                            if (frame is Frame.Text) {
                                val msg = ServerMsg.parse(frame.readText())
                                if (msg is ServerMsg.HelloOk) onConnected(true)
                                onMessage(msg)
                            }
                        }
                    } finally {
                        sender.cancel()
                    }
                }
            } catch (_: Throwable) {
                // fall through to reconnect
            }
            onConnected(false)
            if (!active) break
            attempt++
            val delayMs = minOf(30_000L, 1000L * (1L shl minOf(attempt, 5))) // 2s..30s
            delay(delayMs)
        }
    }

    fun send(json: String) {
        outbox.trySend(Frame.Text(json))
    }

    fun sendAudio(frame: ByteArray) {
        outbox.trySend(Frame.Binary(true, frame))
    }

    fun close() {
        active = false
        outbox.close()
        // This client is single-use — the controller builds a fresh SpawnerClient on
        // every reconnect / settings change. Cancel the scope (kills any pending
        // reconnect and the running session) and release the HTTP engine.
        scope.cancel()
        client.close()
    }
}

/**
 * Build the platform Ktor client with WebSocket support. Android uses the OkHttp
 * engine (ping interval, no read timeout); the browser uses its native WebSocket.
 * TLS, when present, is terminated at the reverse proxy — neither engine handles it.
 */
expect fun spawnerHttpClient(): HttpClient

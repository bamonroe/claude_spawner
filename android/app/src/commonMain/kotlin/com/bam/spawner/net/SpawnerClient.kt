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
                client.webSocket(urlString = url) {
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

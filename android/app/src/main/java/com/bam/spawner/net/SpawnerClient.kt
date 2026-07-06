package com.bam.spawner.net

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.Response
import okhttp3.WebSocket
import okhttp3.WebSocketListener
import okio.ByteString.Companion.toByteString
import java.util.concurrent.TimeUnit

/**
 * WebSocket client to the spawner gateway. Sends the hello handshake (with a
 * stable client_id for resume) on open, surfaces parsed [ServerMsg]s, and
 * automatically reconnects with backoff until [close] is called. Audio is sent
 * as binary frames; everything else is JSON text.
 */
class SpawnerClient(
    private val url: String,
    private val token: String,
    private val clientId: String,
    private val hello: HelloConfig,
    private val onMessage: (ServerMsg) -> Unit,
    private val onConnected: (Boolean) -> Unit,
    // Optional client certificate for mutual-TLS servers (wss:// with a client-CA
    // requirement). Null = no client cert (plain ws:// or one-way wss://).
    private val tls: ClientTls? = null,
) {
    private val http = OkHttpClient.Builder()
        .pingInterval(20, TimeUnit.SECONDS)
        .readTimeout(0, TimeUnit.MILLISECONDS)
        .apply { tls?.let { sslSocketFactory(it.socketFactory, it.trustManager) } }
        .build()

    private val scope = CoroutineScope(Dispatchers.IO + SupervisorJob())

    @Volatile private var ws: WebSocket? = null
    @Volatile private var active = false
    @Volatile private var attempt = 0

    fun connect() {
        active = true
        open()
    }

    private fun open() {
        val req = Request.Builder().url(url).build()
        ws = http.newWebSocket(req, object : WebSocketListener() {
            override fun onOpen(webSocket: WebSocket, response: Response) {
                attempt = 0
                webSocket.send(Outbound.hello(token, clientId, hello))
            }

            override fun onMessage(webSocket: WebSocket, text: String) {
                val msg = ServerMsg.parse(text)
                if (msg is ServerMsg.HelloOk) onConnected(true)
                onMessage(msg)
            }

            override fun onClosed(webSocket: WebSocket, code: Int, reason: String) {
                onConnected(false)
                scheduleReconnect()
            }

            override fun onFailure(webSocket: WebSocket, t: Throwable, response: Response?) {
                onConnected(false)
                scheduleReconnect()
            }
        })
    }

    private fun scheduleReconnect() {
        if (!active) return
        attempt++
        val delayMs = minOf(30_000L, 1000L * (1L shl minOf(attempt, 5))) // 2s..30s
        scope.launch {
            delay(delayMs)
            if (active) open()
        }
    }

    fun send(json: String) {
        ws?.send(json)
    }

    fun sendAudio(frame: ByteArray) {
        ws?.send(frame.toByteString(0, frame.size))
    }

    fun close() {
        active = false
        ws?.close(1000, "bye")
        ws = null
        // This client is single-use — the controller builds a fresh SpawnerClient
        // on every reconnect / settings change. Release the coroutine scope (kills
        // any pending reconnect) and OkHttp's thread + connection pools so the old
        // client doesn't leak them.
        scope.cancel()
        http.dispatcher.executorService.shutdown()
        http.connectionPool.evictAll()
    }
}

package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.js.Js
import io.ktor.client.plugins.websocket.WebSockets

/** Web transport: the browser's native WebSocket via Ktor's Js engine. TLS is owned
 *  by the browser/OS (and terminated at the reverse proxy), so there's nothing to
 *  configure here. */
actual fun spawnerHttpClient(): HttpClient = HttpClient(Js) {
    install(WebSockets)
}

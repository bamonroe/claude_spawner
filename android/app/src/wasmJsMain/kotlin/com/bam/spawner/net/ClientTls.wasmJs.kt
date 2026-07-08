package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.js.Js
import io.ktor.client.plugins.websocket.WebSockets

/** No client-cert material in the browser — TLS (and any mutual-TLS challenge) is
 *  handled by the browser/OS, not by JS. Never instantiated. */
actual class ClientTls private constructor()

/** Web transport: the browser's native WebSocket via Ktor's Js engine. [tls] is
 *  ignored — the browser owns the TLS handshake. */
actual fun spawnerHttpClient(tls: ClientTls?): HttpClient = HttpClient(Js) {
    install(WebSockets)
}

package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.js.Js
import io.ktor.client.plugins.websocket.WebSockets

/** Web transport: the browser's native WebSocket via Ktor's Js engine. TLS is owned
 *  by the browser/OS, so [caPem] can't be applied here (trust a CA in the browser/OS
 *  instead) and is ignored. */
actual fun spawnerHttpClient(caPem: String?): HttpClient = HttpClient(Js) {
    install(WebSockets)
}

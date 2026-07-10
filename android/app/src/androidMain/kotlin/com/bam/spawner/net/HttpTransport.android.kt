package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.websocket.WebSockets
import java.util.concurrent.TimeUnit

/** Android transport: the OkHttp engine with no read timeout (the WebSocket stays
 *  open; the plugin's ping keeps it alive). TLS is terminated at the reverse proxy
 *  (Caddy), so there's no client certificate to install here. */
actual fun spawnerHttpClient(): HttpClient = HttpClient(OkHttp) {
    install(WebSockets) { pingIntervalMillis = 20_000 }
    engine {
        config { readTimeout(0, TimeUnit.MILLISECONDS) }
    }
}

package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.websocket.WebSockets
import java.io.File
import java.security.KeyStore
import java.util.concurrent.TimeUnit
import javax.net.ssl.KeyManagerFactory
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocketFactory
import javax.net.ssl.TrustManager
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

/** OkHttp TLS material for presenting a client certificate (mutual TLS). */
actual class ClientTls(val socketFactory: SSLSocketFactory, val trustManager: X509TrustManager)

/** Android transport: the OkHttp engine, with an optional client cert + no read timeout
 *  (WebSocket stays open; the plugin's ping keeps it alive). */
actual fun spawnerHttpClient(tls: ClientTls?): HttpClient = HttpClient(OkHttp) {
    install(WebSockets) { pingIntervalMillis = 20_000 }
    engine {
        config { readTimeout(0, TimeUnit.MILLISECONDS) }
        if (tls != null) config { sslSocketFactory(tls.socketFactory, tls.trustManager) }
    }
}

/**
 * Build [ClientTls] from a PKCS#12 (.p12/.pfx) file and its passphrase, for a
 * server that demands mutual TLS (SPAWNER_TLS_CLIENT_CA on the server side).
 *
 * The server's own certificate is still verified against the system trust store,
 * so a publicly-trusted server cert (e.g. a Tailscale/Let's Encrypt one) works
 * unchanged — we only ADD a client key for the server's mTLS challenge.
 *
 * Throws if the file is missing/corrupt or the passphrase is wrong; callers
 * should catch and surface that rather than connecting cert-less.
 */
fun buildClientTls(p12: File, passphrase: String): ClientTls {
    val pass = passphrase.toCharArray()
    val keyStore = KeyStore.getInstance("PKCS12").apply {
        p12.inputStream().use { load(it, pass) }
    }
    val kmf = KeyManagerFactory.getInstance(KeyManagerFactory.getDefaultAlgorithm()).apply {
        init(keyStore, pass)
    }
    val tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm()).apply {
        init(null as KeyStore?) // system CAs — verify the server the normal way
    }
    val trustManager = tmf.trustManagers.filterIsInstance<X509TrustManager>().first()
    val ctx = SSLContext.getInstance("TLS").apply {
        init(kmf.keyManagers, arrayOf<TrustManager>(trustManager), null)
    }
    return ClientTls(ctx.socketFactory, trustManager)
}

package com.bam.spawner.net

import io.ktor.client.HttpClient
import io.ktor.client.engine.okhttp.OkHttp
import io.ktor.client.plugins.websocket.WebSockets
import java.security.KeyStore
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.util.concurrent.TimeUnit
import javax.net.ssl.SSLContext
import javax.net.ssl.TrustManagerFactory
import javax.net.ssl.X509TrustManager

/** Android transport: the OkHttp engine with no read timeout (the WebSocket stays
 *  open; the plugin's ping keeps it alive). When [caPem] is set we add it as a trust
 *  anchor on top of the system CAs, so a `wss://` server with a private cert (Caddy
 *  `tls internal`) is reachable while public certs still validate. */
actual fun spawnerHttpClient(caPem: String?): HttpClient = HttpClient(OkHttp) {
    install(WebSockets) { pingIntervalMillis = 20_000 }
    engine {
        config { readTimeout(0, TimeUnit.MILLISECONDS) }
        buildCaTrust(caPem)?.let { (sf, tm) ->
            config { sslSocketFactory(sf, tm) }
        }
    }
}

/** Parse [caPem] (one or more PEM certificates) and return an SSL socket factory +
 *  trust manager that trust those CAs *in addition* to the platform defaults. Returns
 *  null when [caPem] is blank or unparseable (fall back to the default OkHttp trust). */
private fun buildCaTrust(caPem: String?): Pair<javax.net.ssl.SSLSocketFactory, X509TrustManager>? {
    val pem = caPem?.trim().orEmpty()
    if (pem.isEmpty()) return null
    return try {
        val cf = CertificateFactory.getInstance("X.509")
        val certs = pem.byteInputStream().use { cf.generateCertificates(it) }
            .filterIsInstance<X509Certificate>()
        if (certs.isEmpty()) return null

        // Custom trust manager over a keystore holding just our CA(s)…
        val ks = KeyStore.getInstance(KeyStore.getDefaultType()).apply {
            load(null, null)
            certs.forEachIndexed { i, c -> setCertificateEntry("ca$i", c) }
        }
        val custom = trustManagerFrom(ks)
        // …and the platform's default trust manager (system CAs), so public certs
        // keep validating alongside the private one.
        val system = trustManagerFrom(null)

        val composite = CompositeX509TrustManager(listOf(system, custom))
        val ctx = SSLContext.getInstance("TLS").apply { init(null, arrayOf(composite), null) }
        ctx.socketFactory to composite
    } catch (_: Exception) {
        null
    }
}

private fun trustManagerFrom(ks: KeyStore?): X509TrustManager {
    val tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
    tmf.init(ks)
    return tmf.trustManagers.filterIsInstance<X509TrustManager>().first()
}

/** Trusts a server if *any* delegate does; used to add a private CA without dropping
 *  the system trust store. Client-auth is unused (we never present a client cert). */
private class CompositeX509TrustManager(
    private val delegates: List<X509TrustManager>,
) : X509TrustManager {
    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        var last: Exception? = null
        for (tm in delegates) {
            try {
                tm.checkServerTrusted(chain, authType)
                return
            } catch (e: Exception) {
                last = e
            }
        }
        throw last ?: java.security.cert.CertificateException("no trust manager accepted the chain")
    }

    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {
        delegates.first().checkClientTrusted(chain, authType)
    }

    override fun getAcceptedIssuers(): Array<X509Certificate> =
        delegates.flatMap { it.acceptedIssuers.asList() }.toTypedArray()
}

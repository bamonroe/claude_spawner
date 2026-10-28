package com.bam.spawner

import com.bam.spawner.net.ClientInfo

actual fun platformName(): String = "Web"

private fun userAgent(): String = js("(typeof navigator !== 'undefined' && navigator.userAgent) || ''")

/** The hello handshake's client identity: the bundle's build stamp + the browser's UA. */
fun webClientInfo(): ClientInfo = ClientInfo(
    appVersion = "${BuildInfo.versionName} (${BuildInfo.versionCode}) ${BuildInfo.gitCommitShort}",
    device = userAgent().ifBlank { "browser" },
    platform = "web",
)

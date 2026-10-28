package com.bam.spawner

import android.content.Context
import android.os.Build
import com.bam.spawner.net.ClientInfo

actual fun platformName(): String = "Android"

/**
 * The hello handshake's client identity. The version comes from the installed
 * package (its versionCode is stamped per build by the builder's init script, so
 * [BuildInfo.versionCode] would be stale), plus the commit the bundle was built from.
 */
fun androidClientInfo(context: Context): ClientInfo {
    val (name, code) = try {
        val pi = context.packageManager.getPackageInfo(context.packageName, 0)
        val vc = if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) pi.longVersionCode
        else @Suppress("DEPRECATION") pi.versionCode.toLong()
        (pi.versionName ?: BuildInfo.versionName) to vc
    } catch (e: Exception) {
        BuildInfo.versionName to BuildInfo.versionCode.toLong()
    }
    val maker = Build.MANUFACTURER.replaceFirstChar { it.uppercase() }
    val model = if (Build.MODEL.startsWith(Build.MANUFACTURER, ignoreCase = true)) Build.MODEL
    else "$maker ${Build.MODEL}"
    return ClientInfo(
        appVersion = "$name ($code) ${BuildInfo.gitCommitShort}",
        device = "$model, Android ${Build.VERSION.RELEASE}",
        platform = "android",
    )
}

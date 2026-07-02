package com.bam.spawner

import android.content.Context

/**
 * Process-wide holder for the single [VoiceController] so MainActivity and
 * VoiceService drive the same connection + recorder (one mic, one client).
 * Survives Activity recreation (rotation); lives for the process lifetime.
 */
object Spawner {
    @Volatile private var ctrl: VoiceController? = null

    fun controller(context: Context): VoiceController =
        ctrl ?: synchronized(this) {
            ctrl ?: run {
                val app = context.applicationContext
                VoiceController(app, SettingsStore(app)).also { ctrl = it }
            }
        }
}

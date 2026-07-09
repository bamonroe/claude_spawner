package com.bam.spawner.audio

import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.VolumeOff
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.Bluetooth
import androidx.compose.material.icons.filled.Phone
import androidx.compose.ui.graphics.vector.ImageVector

/** A selectable audio output for the spoken (TTS) path. MUTE isn't a device — it
 *  suppresses TTS entirely (handled by the caller, not routed here). Shared so the
 *  top-bar output picker renders identically on both clients; the actual routing
 *  (`AudioRouter`) stays Android-only behind the controller. The [icon] is a Material
 *  vector so it renders on every target (the browser has no system emoji font). */
enum class AudioOutput(val label: String, val icon: ImageVector) {
    EARPIECE("Earpiece", Icons.Filled.Phone),
    SPEAKER("Speaker", Icons.AutoMirrored.Filled.VolumeUp),
    BLUETOOTH("Bluetooth", Icons.Filled.Bluetooth),
    MUTE("Mute", Icons.AutoMirrored.Filled.VolumeOff),
}

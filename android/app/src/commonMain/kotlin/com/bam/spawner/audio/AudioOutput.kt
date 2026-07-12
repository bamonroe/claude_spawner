package com.bam.spawner.audio

import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.VolumeOff
import androidx.compose.material.icons.automirrored.filled.VolumeUp
import androidx.compose.material.icons.filled.Headset
import androidx.compose.material.icons.filled.Phone
import androidx.compose.ui.graphics.vector.ImageVector

/** A selectable audio output for the spoken (TTS) path — where Claude's voice plays.
 *  Chosen independently of the capture source ([AudioInput]); the two together fully
 *  determine the capture profile with no inference (see VoiceController.resolveMicProfile).
 *  MUTE isn't a device — it suppresses TTS entirely (handled by the caller, not routed
 *  here). "Headset" is a connected headset's own speaker at full media (A2DP) quality;
 *  picking the headset *mic* (an [AudioInput]) instead grabs its call link, which then
 *  carries playback too. Shared so the top-bar picker renders identically on both
 *  clients; the actual routing (`AudioRouter`) stays Android-only behind the controller.
 *  The [icon] is a Material vector so it renders on every target (the browser has no
 *  system emoji font). */
enum class AudioOutput(val label: String, val icon: ImageVector) {
    EARPIECE("Earpiece", Icons.Filled.Phone),
    SPEAKER("Speaker", Icons.AutoMirrored.Filled.VolumeUp),
    // Full-quality media (A2DP) to connected headphones. Offered only while a headset
    // is connected; the preferred default when one is.
    HEADSET("Headset", Icons.Filled.Headset),
    MUTE("Mute", Icons.AutoMirrored.Filled.VolumeOff),
}

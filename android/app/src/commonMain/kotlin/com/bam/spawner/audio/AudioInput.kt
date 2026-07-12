package com.bam.spawner.audio

import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Bluetooth
import androidx.compose.material.icons.filled.Mic
import androidx.compose.ui.graphics.vector.ImageVector

/** A selectable capture (microphone) source for hands-free listening, chosen
 *  independently of the [AudioOutput]. Explicit input + output together fully
 *  determine the capture profile — comm-mode vs media, mic source, echo canceller —
 *  with no inference (see VoiceController.resolveMicProfile). Shared so the top-bar
 *  picker renders identically on every client; the actual routing (`AudioRouter`)
 *  stays Android-only behind the controller. The [icon] is a Material vector so it
 *  renders on every target. */
enum class AudioInput(val label: String, val icon: ImageVector) {
    // The device's own built-in microphone.
    DEVICE("Device", Icons.Filled.Mic),

    // A paired Bluetooth headset's own mic over its hands-free (SCO) link — call-mode
    // audio + echo canceller, heard from across the room at call quality. The SCO link
    // is bidirectional, so this also carries playback to the headset. Offered only
    // while such a headset is connected.
    HEADSET("Headset", Icons.Filled.Bluetooth);

    /** Persisted form for `Prefs.micSource`; legacy value "phone" == [DEVICE]. */
    val pref: String get() = if (this == HEADSET) "headset" else "phone"

    companion object {
        fun fromPref(s: String): AudioInput = if (s == "headset") HEADSET else DEVICE
    }
}

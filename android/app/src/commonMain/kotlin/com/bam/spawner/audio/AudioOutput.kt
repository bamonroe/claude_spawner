package com.bam.spawner.audio

/** A selectable audio output for the spoken (TTS) path. MUTE isn't a device — it
 *  suppresses TTS entirely (handled by the caller, not routed here). Shared so the
 *  top-bar output picker renders identically on both clients; the actual routing
 *  (`AudioRouter`) stays Android-only behind the controller. */
enum class AudioOutput(val label: String, val icon: String) {
    EARPIECE("Earpiece", "📞"),
    SPEAKER("Speaker", "🔊"),
    BLUETOOTH("Bluetooth", "🔵"),
    MUTE("Mute", "🔇"),
}

package com.bam.spawner.audio

import android.content.Context
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.os.Build

/** A selectable audio output for the spoken (TTS) path. MUTE isn't a device — it
 *  suppresses TTS entirely (handled by the caller, not routed here). */
enum class AudioOutput(val label: String, val icon: String) {
    EARPIECE("Earpiece", "📞"),
    SPEAKER("Speaker", "🔊"),
    BLUETOOTH("Bluetooth", "🔵"),
    MUTE("Mute", "🔇"),
}

/**
 * Routes the communication-audio stream to a chosen output. The TTS speaks with
 * USAGE_VOICE_COMMUNICATION (so the platform echo canceller can cancel it from the
 * mic during hands-free), which the platform routes to the earpiece by default —
 * this lets the user pick earpiece / speaker / Bluetooth instead.
 *
 * On API 31+ it uses AudioManager.setCommunicationDevice, which redirects exactly
 * that communication stream while keeping the echo-cancelling pipeline. Older
 * devices fall back to the speakerphone toggle (earpiece/speaker only — Bluetooth
 * isn't offered there).
 */
class AudioRouter(context: Context) {
    private val am = context.applicationContext.getSystemService(AudioManager::class.java)

    /**
     * Outputs currently available: earpiece and speaker are always present;
     * Bluetooth appears only while a Bluetooth communication device is connected.
     */
    fun available(): List<AudioOutput> {
        val outs = mutableListOf(AudioOutput.EARPIECE, AudioOutput.SPEAKER)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && am != null) {
            if (am.availableCommunicationDevices.any { it.isBluetooth() }) {
                outs.add(AudioOutput.BLUETOOTH)
            }
        }
        outs.add(AudioOutput.MUTE) // always offer mute
        return outs
    }

    /** The output currently in effect (best-effort; EARPIECE if unknown). */
    fun current(): AudioOutput {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && am != null) {
            val dev = am.communicationDevice ?: return AudioOutput.EARPIECE
            return when {
                dev.isBluetooth() -> AudioOutput.BLUETOOTH
                dev.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> AudioOutput.SPEAKER
                else -> AudioOutput.EARPIECE
            }
        }
        @Suppress("DEPRECATION")
        return if (am?.isSpeakerphoneOn == true) AudioOutput.SPEAKER else AudioOutput.EARPIECE
    }

    /** Route the communication stream to [out]. Returns true if it took effect.
     *  MUTE is not a device — the caller suppresses TTS instead of routing here. */
    fun setOutput(out: AudioOutput): Boolean {
        if (out == AudioOutput.MUTE) return true
        val am = am ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val dev = am.availableCommunicationDevices.firstOrNull {
                when (out) {
                    AudioOutput.EARPIECE -> it.type == AudioDeviceInfo.TYPE_BUILTIN_EARPIECE
                    AudioOutput.SPEAKER -> it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER
                    AudioOutput.BLUETOOTH -> it.isBluetooth()
                    AudioOutput.MUTE -> false // unreachable (guarded above)
                }
            } ?: return false
            return try {
                am.setCommunicationDevice(dev)
            } catch (e: SecurityException) {
                false // routing to a Bluetooth device can need BLUETOOTH_CONNECT
            }
        }
        // Legacy (< API 31): only earpiece/speaker, via the speakerphone toggle.
        return try {
            am.mode = AudioManager.MODE_IN_COMMUNICATION
            @Suppress("DEPRECATION")
            am.isSpeakerphoneOn = (out == AudioOutput.SPEAKER)
            out != AudioOutput.BLUETOOTH
        } catch (e: Exception) {
            false
        }
    }

    private fun AudioDeviceInfo.isBluetooth() =
        type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO ||
            type == AudioDeviceInfo.TYPE_BLE_HEADSET ||
            type == AudioDeviceInfo.TYPE_HEARING_AID
}

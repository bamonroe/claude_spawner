package com.bam.spawner.audio

import android.content.Context
import android.media.AudioDeviceCallback
import android.media.AudioDeviceInfo
import android.media.AudioManager
import android.os.Build

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
        if (headphonesConnected()) outs.add(AudioOutput.HEADSET)
        outs.add(AudioOutput.MUTE) // always offer mute
        return outs
    }

    /** Capture sources currently available: the device's own mic is always present;
     *  the headset mic appears only while a Bluetooth headset with a mic is paired. */
    fun availableInputs(): List<AudioInput> {
        val ins = mutableListOf(AudioInput.DEVICE)
        if (bluetoothMicAvailable()) ins.add(AudioInput.HEADSET)
        return ins
    }

    /** The output currently in effect (best-effort; EARPIECE if unknown). */
    fun current(): AudioOutput {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S && am != null) {
            val dev = am.communicationDevice ?: return AudioOutput.EARPIECE
            return when {
                // A grabbed Bluetooth comm device means the headset link is in use for
                // playback (the headset-mic case), which we surface as the headset output.
                dev.isBluetooth() -> AudioOutput.HEADSET
                dev.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER -> AudioOutput.SPEAKER
                else -> AudioOutput.EARPIECE
            }
        }
        @Suppress("DEPRECATION")
        return if (am?.isSpeakerphoneOn == true) AudioOutput.SPEAKER else AudioOutput.EARPIECE
    }

    /** Best-effort post-route verification. For headset-media output we deliberately
     *  release the communication device; headphones being present is the route signal. */
    fun outputActive(out: AudioOutput): Boolean = when (out) {
        AudioOutput.MUTE -> true
        AudioOutput.HEADSET -> headphonesConnected()
        else -> current() == out
    }

    /** Route the communication stream to [out]. Returns true if it took effect.
     *  MUTE is not a device — the caller suppresses TTS instead of routing here. */
    fun setOutput(out: AudioOutput): Boolean {
        if (out == AudioOutput.MUTE) return true
        val am = am ?: return false
        // Headset-media: leave the communication stream unrouted so TTS plays as plain
        // media over the connected headphones (full A2DP quality); capture then uses
        // the built-in mic with no call mode. Releasing any grabbed comm device is what
        // keeps us out of the SCO downgrade / far-field gain clamp.
        if (out == AudioOutput.HEADSET) {
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
                runCatching { am.clearCommunicationDevice() }
            } else {
                @Suppress("DEPRECATION")
                runCatching { am.isSpeakerphoneOn = false; am.mode = AudioManager.MODE_NORMAL }
            }
            return true
        }
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val dev = am.availableCommunicationDevices.firstOrNull {
                when (out) {
                    AudioOutput.EARPIECE -> it.type == AudioDeviceInfo.TYPE_BUILTIN_EARPIECE
                    AudioOutput.SPEAKER -> it.type == AudioDeviceInfo.TYPE_BUILTIN_SPEAKER
                    AudioOutput.HEADSET, AudioOutput.MUTE -> false // unreachable (guarded above)
                }
            } ?: return false
            return try {
                am.setCommunicationDevice(dev)
            } catch (e: SecurityException) {
                false
            }
        }
        // Legacy (< API 31): only earpiece/speaker, via the speakerphone toggle.
        return try {
            am.mode = AudioManager.MODE_IN_COMMUNICATION
            @Suppress("DEPRECATION")
            am.isSpeakerphoneOn = (out == AudioOutput.SPEAKER)
            true
        } catch (e: Exception) {
            false
        }
    }

    /**
     * Notify [onChange] whenever an output device is added or removed (headphones
     * plugged/unplugged, Bluetooth connected/dropped) so the caller can re-resolve
     * the hands-free audio mode live. The platform delivers the callback on the main
     * thread, so [onChange] should hand off any blocking work to a background scope.
     */
    fun registerRouteCallback(onChange: () -> Unit) {
        val am = am ?: return
        val cb = object : AudioDeviceCallback() {
            override fun onAudioDevicesAdded(added: Array<out AudioDeviceInfo>?) = onChange()
            override fun onAudioDevicesRemoved(removed: Array<out AudioDeviceInfo>?) = onChange()
        }
        am.registerAudioDeviceCallback(cb, null)
    }

    /**
     * True when spoken audio is currently going to headphones (wired, USB, or a
     * Bluetooth/BLE headset) rather than the built-in speaker/earpiece. When it is,
     * our TTS is in the user's ears with negligible leakage into the mic, so
     * hands-free can run in plain media mode (no call-mode ducking of other apps'
     * audio, no echo canceller) instead of the barge-in comm-audio setup.
     */
    fun headphonesConnected(): Boolean {
        val am = am ?: return false
        return am.getDevices(AudioManager.GET_DEVICES_OUTPUTS).any { it.isHeadphone() }
    }

    /** True when a paired Bluetooth headset with a microphone (its hands-free/SCO or
     *  BLE profile) is available to capture from. */
    fun bluetoothMicAvailable(): Boolean {
        val am = am ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            return am.availableCommunicationDevices.any { it.isBluetoothMic() }
        }
        @Suppress("DEPRECATION")
        return am.isBluetoothScoAvailableOffCall
    }

    /** Route capture (and playback) through a Bluetooth headset's own mic via its
     *  hands-free profile. This is call-mode audio — it ducks other apps and drops
     *  the headset to call quality — but it lets the user be heard from across the
     *  room. Returns true if it engaged; pair with [disableHeadsetMic] to release it. */
    fun enableHeadsetMic(): Boolean {
        val am = am ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            val dev = am.availableCommunicationDevices.firstOrNull { it.isBluetoothMic() } ?: return false
            return try {
                am.setCommunicationDevice(dev)
            } catch (e: SecurityException) {
                false // needs BLUETOOTH_CONNECT
            }
        }
        return try {
            am.mode = AudioManager.MODE_IN_COMMUNICATION
            @Suppress("DEPRECATION")
            am.startBluetoothSco()
            @Suppress("DEPRECATION")
            am.isBluetoothScoOn = true
            true
        } catch (e: Exception) {
            false
        }
    }

    /** Whether the Bluetooth hands-free (SCO) link is actually carrying audio right
     *  now — not merely requested. [enableHeadsetMic] returns as soon as the request
     *  is accepted, but the physical SCO link comes up (or fails) a beat later; when
     *  it fails the platform reverts the communication device to A2DP (no mic), so a
     *  short while after enabling, this tells the caller whether the headset mic is
     *  really live or it should fall back to the built-in mic. */
    fun headsetMicActive(): Boolean {
        val am = am ?: return false
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            return am.communicationDevice?.isBluetoothMic() ?: false
        }
        @Suppress("DEPRECATION")
        return am.isBluetoothScoOn
    }

    /** Release the Bluetooth hands-free profile grabbed by [enableHeadsetMic]. */
    fun disableHeadsetMic() {
        val am = am ?: return
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            runCatching { am.clearCommunicationDevice() }
        } else {
            @Suppress("DEPRECATION")
            runCatching { am.isBluetoothScoOn = false; am.stopBluetoothSco() }
        }
    }

    private fun AudioDeviceInfo.isBluetoothMic() =
        type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO || type == AudioDeviceInfo.TYPE_BLE_HEADSET

    private fun AudioDeviceInfo.isHeadphone() = when (type) {
        AudioDeviceInfo.TYPE_WIRED_HEADPHONES,
        AudioDeviceInfo.TYPE_WIRED_HEADSET,
        AudioDeviceInfo.TYPE_USB_HEADSET,
        AudioDeviceInfo.TYPE_BLUETOOTH_A2DP,
        AudioDeviceInfo.TYPE_BLE_HEADSET,
        AudioDeviceInfo.TYPE_BLE_SPEAKER,
        AudioDeviceInfo.TYPE_BLUETOOTH_SCO,
        AudioDeviceInfo.TYPE_HEARING_AID,
        -> true
        else -> false
    }

    private fun AudioDeviceInfo.isBluetooth() =
        type == AudioDeviceInfo.TYPE_BLUETOOTH_SCO ||
            type == AudioDeviceInfo.TYPE_BLE_HEADSET ||
            type == AudioDeviceInfo.TYPE_HEARING_AID
}

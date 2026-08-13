package com.bam.spawner

import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * The mic held open by the radial palette's centre button: one tap-to-speak capture that
 * keeps recording without a finger down, ending only when the lock is explicitly released
 * (send) or abandoned (drop the clip). Not hands-free — no wake word, no VAD, one clip.
 *
 * The lock lives **with the recorder**, on the process-scoped controller, not in the UI.
 * A locked mic is a property of the capture that's running, so it has to outlive any one
 * composition or Activity instance: state remembered in the screen was lost whenever
 * Android recreated the Activity (rotation), leaving the controller still recording with
 * no on-screen way to end it. Both clients build one of these over their own capture
 * calls, so the lock behaves identically on the phone and in the browser.
 */
class MicLock(
    private val startCapture: () -> Unit,
    private val stopCapture: () -> Unit,
    private val cancelCapture: () -> Unit,
) {
    private val _locked = MutableStateFlow(false)
    val locked: StateFlow<Boolean> = _locked.asStateFlow()

    /** Hold the mic open and start capturing. No-op when already locked. */
    fun lock() {
        if (_locked.value) return
        _locked.value = true
        startCapture()
    }

    /** Release the lock and send what's been captured. Safe to call when unlocked. */
    fun release() {
        if (!_locked.value) return
        _locked.value = false
        stopCapture()
    }

    /** Drop the lock and throw the clip away — used when the capture can no longer be
     *  delivered as recorded (the socket dropped, or hands-free is taking the mic). */
    fun abandon() {
        if (!_locked.value) return
        _locked.value = false
        cancelCapture()
    }
}

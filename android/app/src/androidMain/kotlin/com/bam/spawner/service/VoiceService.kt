package com.bam.spawner.service

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.Service
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import com.bam.spawner.Spawner
import com.bam.spawner.platformLog
import com.bam.spawner.startHandsFree
import com.bam.spawner.stopHandsFree

/**
 * Foreground service for hands-free listening. Its microphone foreground type is
 * what lets the app keep capturing while briefly backgrounded / screen-off. It
 * drives the shared [com.bam.spawner.VoiceController] (via [Spawner]) so the UI
 * and the service share one mic + connection.
 */
class VoiceService : Service() {
    override fun onBind(intent: Intent?): IBinder? = null

    // Re-sent by the Activity after a location grant/toggle: startForegroundCompat
    // re-promotes with the current types and startHandsFree is a no-op when running.
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForegroundCompat()
        Spawner.controller(this).startHandsFree()
        return START_STICKY
    }

    override fun onDestroy() {
        Spawner.controller(this).stopHandsFree()
        super.onDestroy()
    }

    private fun startForegroundCompat() {
        val channelId = "voice"
        val nm = getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            nm.createNotificationChannel(
                NotificationChannel(channelId, "Voice", NotificationManager.IMPORTANCE_LOW),
            )
        }
        val notification: Notification = Notification.Builder(this, channelId)
            .setContentTitle("Claude Spawner")
            .setContentText("Listening — say \"hey buddy\"")
            .setSmallIcon(android.R.drawable.ic_btn_speak_now)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            val mic = ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
            // The `location` type is what lets the low-power location updates keep
            // arriving while backgrounded/screen-off. Android 14+ throws a
            // SecurityException if it's claimed without the permission (or from a
            // background start), so add it only when tagging is on and granted, and
            // fall back to microphone-only rather than ever crashing the service.
            val ctrl = Spawner.controller(this)
            if (ctrl.settings.tagLocation && ctrl.location.hasPermission()) {
                try {
                    startForeground(1, notification, mic or ServiceInfo.FOREGROUND_SERVICE_TYPE_LOCATION)
                    return
                } catch (e: SecurityException) {
                    platformLog("voice service: location FGS type refused, mic only: ${e.message}")
                }
            }
            startForeground(1, notification, mic)
        } else {
            startForeground(1, notification)
        }
    }
}

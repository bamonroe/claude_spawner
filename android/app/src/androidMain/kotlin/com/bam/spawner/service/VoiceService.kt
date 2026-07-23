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
            startForeground(1, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE)
        } else {
            startForeground(1, notification)
        }
    }
}

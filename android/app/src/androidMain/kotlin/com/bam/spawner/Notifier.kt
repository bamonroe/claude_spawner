package com.bam.spawner

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context

/**
 * Posts a local notification when a turn finishes while the app is backgrounded,
 * so a long turn run from your pocket surfaces its reply. Tapping it opens the app.
 */
class Notifier(context: Context) {
    private val app = context.applicationContext
    private val nm = app.getSystemService(NotificationManager::class.java)
    private val channelId = "turn_replies"

    init {
        nm?.createNotificationChannel(
            NotificationChannel(channelId, "Session replies", NotificationManager.IMPORTANCE_DEFAULT),
        )
    }

    fun turnDone(session: String, reply: String) {
        val nm = nm ?: return
        val launch = app.packageManager.getLaunchIntentForPackage(app.packageName)
        val pi = launch?.let {
            PendingIntent.getActivity(app, 0, it, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        }
        val builder = Notification.Builder(app, channelId)
            .setSmallIcon(android.R.drawable.ic_dialog_info)
            .setContentTitle(if (session.isBlank()) "Claude replied" else "Claude · $session")
            .setContentText(reply.take(120))
            .setStyle(Notification.BigTextStyle().bigText(reply.take(500)))
            .setAutoCancel(true)
        if (pi != null) builder.setContentIntent(pi)
        // One notification per session (later replies replace the earlier one).
        nm.notify(if (session.isBlank()) 0 else session.hashCode(), builder.build())
    }
}

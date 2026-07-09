package com.bam.spawner

import android.os.SystemClock
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

actual fun nowMonotonicMs(): Long = SystemClock.elapsedRealtime()

actual fun nowEpochSeconds(): Long = System.currentTimeMillis() / 1000

actual fun fmtClock(unixSeconds: Long): String =
    SimpleDateFormat("h:mm a", Locale.getDefault()).format(Date(unixSeconds * 1000))

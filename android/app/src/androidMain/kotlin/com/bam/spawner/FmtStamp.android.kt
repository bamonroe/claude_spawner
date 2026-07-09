package com.bam.spawner

import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

actual fun fmtStamp(unixSeconds: Long): String =
    SimpleDateFormat("MMM d, h:mm a", Locale.getDefault()).format(Date(unixSeconds * 1000))

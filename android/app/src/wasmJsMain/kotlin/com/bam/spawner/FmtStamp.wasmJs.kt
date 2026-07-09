package com.bam.spawner

// Format via the browser's Intl (JS Date#toLocaleString) → e.g. "Jul 8, 3:45 PM",
// matching the Android SimpleDateFormat badge closely enough.
private fun jsFormatStamp(ms: Double): String =
    js("new Date(ms).toLocaleString('en-US', {month:'short', day:'numeric', hour:'numeric', minute:'2-digit'})")

actual fun fmtStamp(unixSeconds: Long): String = jsFormatStamp(unixSeconds.toDouble() * 1000)

package com.bam.spawner

/**
 * One diagnostic line to the platform's log — logcat on Android, the browser
 * console on web. Deliberately minimal: this is a seam for instrumentation
 * (attach latency, and whatever measurement comes next), not a logging
 * framework, and nothing user-facing should route through it.
 */
expect fun platformLog(line: String)

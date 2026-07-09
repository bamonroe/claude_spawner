package com.bam.spawner

import kotlin.time.TimeSource

// A process-start monotonic mark; elapsed since it gives a clock comparable across calls.
private val start = TimeSource.Monotonic.markNow()

actual fun nowMonotonicMs(): Long = start.elapsedNow().inWholeMilliseconds

private fun jsNowMs(): Double = js("Date.now()")

actual fun nowEpochSeconds(): Long = (jsNowMs() / 1000.0).toLong()

private fun jsFormatClock(ms: Double): String =
    js("new Date(ms).toLocaleTimeString('en-US', {hour:'numeric', minute:'2-digit'})")

actual fun fmtClock(unixSeconds: Long): String = jsFormatClock(unixSeconds.toDouble() * 1000)

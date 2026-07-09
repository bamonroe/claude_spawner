package com.bam.spawner

import kotlin.time.TimeSource

// A process-start monotonic mark; elapsed since it gives a clock comparable across calls.
private val start = TimeSource.Monotonic.markNow()

actual fun nowMonotonicMs(): Long = start.elapsedNow().inWholeMilliseconds

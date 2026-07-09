package com.bam.spawner

/**
 * Milliseconds from a monotonic clock — for measuring *elapsed* durations only (never
 * wall-clock time). Used to stamp [TurnUsageInfo.atElapsedMs] and to count down the
 * ~5-min warm prompt-cache window in the UI, so the two must read the same clock.
 * Android backs it with `SystemClock.elapsedRealtime()`; web with the monotonic
 * `TimeSource`.
 */
expect fun nowMonotonicMs(): Long

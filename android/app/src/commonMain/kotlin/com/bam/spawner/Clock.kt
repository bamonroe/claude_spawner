package com.bam.spawner

/**
 * Milliseconds from a monotonic clock — for measuring *elapsed* durations only (never
 * wall-clock time). Used to stamp [TurnUsageInfo.atElapsedMs] and to count down the
 * ~5-min warm prompt-cache window in the UI, so the two must read the same clock.
 * Android backs it with `SystemClock.elapsedRealtime()`; web with the monotonic
 * `TimeSource`.
 */
expect fun nowMonotonicMs(): Long

/** Current wall-clock time in unix seconds — for "2h ago" / "resets in …" relative labels. */
expect fun nowEpochSeconds(): Long

/**
 * Current wall-clock time in unix MILLISECONDS — used to stamp `updated_at` on a
 * locally-edited catalogue record (host/identity/profile/provider) at the moment it
 * is pushed to the server, so the versioned last-writer-wins sync can arbitrate.
 */
expect fun nowEpochMs(): Long

/** A unix-seconds instant as a local time-of-day, e.g. "3:45 PM". */
expect fun fmtClock(unixSeconds: Long): String

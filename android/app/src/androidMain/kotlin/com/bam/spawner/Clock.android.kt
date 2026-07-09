package com.bam.spawner

import android.os.SystemClock

actual fun nowMonotonicMs(): Long = SystemClock.elapsedRealtime()

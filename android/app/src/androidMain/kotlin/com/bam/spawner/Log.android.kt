package com.bam.spawner

import android.util.Log

actual fun platformLog(line: String) {
    Log.i("Spawner", line)
}

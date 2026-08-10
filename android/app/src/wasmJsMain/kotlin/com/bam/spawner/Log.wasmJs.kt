package com.bam.spawner

private fun consoleLog(line: String): Unit = js("console.log(line)")

actual fun platformLog(line: String) {
    consoleLog(line)
}

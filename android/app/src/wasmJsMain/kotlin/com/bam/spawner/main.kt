package com.bam.spawner

import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.window.ComposeViewport
import kotlinx.browser.document

/** Browser entry point: mount the shared UI ([WebRoot]) into the page body via Kotlin/Wasm. */
@OptIn(ExperimentalComposeUiApi::class)
fun main() {
    ComposeViewport(document.body!!) {
        WebRoot()
    }
}

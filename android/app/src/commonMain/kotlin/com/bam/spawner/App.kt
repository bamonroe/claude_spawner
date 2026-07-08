package com.bam.spawner

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier

/**
 * The shared root composable. Rendered on Android (from MainActivity) and in the browser
 * (from the wasmJs entry point) so both clients draw the exact same UI — the whole point of
 * the Compose Multiplatform migration. Real screens (chat, sidebar, settings) move in here in
 * M3; for now it is a placeholder that proves the shared → both-targets pipeline compiles.
 */
@Composable
fun App() {
    MaterialTheme {
        Surface(modifier = Modifier.fillMaxSize()) {
            Box(modifier = Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                Text("claude_spawner — shared Compose UI (${platformName()})")
            }
        }
    }
}

/** Which platform is drawing us — proves expect/actual resolves per target. */
expect fun platformName(): String

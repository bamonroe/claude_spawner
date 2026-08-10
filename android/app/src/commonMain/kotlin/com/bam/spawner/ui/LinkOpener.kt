package com.bam.spawner.ui

import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.platform.LocalClipboardManager
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.text.AnnotatedString
import com.bam.spawner.platformLog

/**
 * Opens a link in the user's browser, with a copy-to-clipboard fallback.
 *
 * The Claude OAuth flow's redirect target is a hosted Anthropic callback page,
 * not localhost, so the browser does not have to live on the server's machine —
 * whichever device is running the client can handle the login and the user
 * pastes the resulting code back. That makes this the whole platform seam:
 * Compose's [LocalUriHandler] already fires an ACTION_VIEW intent on Android and
 * a new tab on web, so no expect/actual is needed. [copy] is the fallback for
 * when no browser is reachable (or the user wants the link on another device).
 */
interface LinkOpener {
    /** Try to open [url] in a browser. Returns false if nothing could handle it. */
    fun open(url: String): Boolean

    /** Put [text] on the system clipboard. Returns false if that failed. */
    fun copy(text: String): Boolean
}

@Composable
fun rememberLinkOpener(): LinkOpener {
    val uriHandler = LocalUriHandler.current
    val clipboard = LocalClipboardManager.current
    return remember(uriHandler, clipboard) {
        object : LinkOpener {
            override fun open(url: String): Boolean =
                try {
                    uriHandler.openUri(url)
                    true
                } catch (t: Throwable) {
                    platformLog("link open failed: $t")
                    false
                }

            override fun copy(text: String): Boolean =
                try {
                    clipboard.setText(AnnotatedString(text))
                    true
                } catch (t: Throwable) {
                    platformLog("clipboard copy failed: $t")
                    false
                }
        }
    }
}

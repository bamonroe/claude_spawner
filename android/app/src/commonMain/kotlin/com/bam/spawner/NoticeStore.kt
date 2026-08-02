package com.bam.spawner

import com.bam.spawner.net.ServerMsg
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow

/**
 * The shared holder behind [AppController.notices]: the small set of out-of-band
 * heads-ups about sessions the user is not viewing (the server's `notice` frame).
 *
 * It lives in `commonMain` and is owned by both controllers so the Android and web
 * clients can't drift on the one rule that matters here: **at most one notice per
 * session**. A session that finishes three background jobs while you're elsewhere
 * should replace its banner, not stack three of them — the newest summary is the
 * only one worth showing, and unbounded growth would bury the UI.
 */
class NoticeStore {
    private val _notices = MutableStateFlow<List<ServerMsg.Notice>>(emptyList())
    val notices: StateFlow<List<ServerMsg.Notice>> = _notices.asStateFlow()

    /** Record [notice], replacing any earlier one for the same session (newest last). */
    fun add(notice: ServerMsg.Notice) {
        _notices.value = _notices.value.filterNot { it.sessionId == notice.sessionId } + notice
    }

    /** Drop [sessionId]'s notice — the banner was dismissed, or its session was opened. */
    fun dismiss(sessionId: String) {
        _notices.value = _notices.value.filterNot { it.sessionId == sessionId }
    }
}

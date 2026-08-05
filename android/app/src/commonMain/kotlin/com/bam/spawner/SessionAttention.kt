package com.bam.spawner

import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.mutableStateMapOf
import androidx.compose.runtime.remember

/**
 * One definition of "this session wants your attention" — the orange cue — shared by every
 * surface that shows sessions (the sidebar list and the radial palette's ring slots). Keep the
 * predicate here rather than re-deriving it per surface, so the two can never disagree about
 * which sessions are orange.
 */

/** A session's stable key: its session id, falling back to its dir before one is assigned. */
fun sessionKey(d: DiscoveredInfo): String = d.sessionId.ifBlank { d.dir }

/** True for the session you're currently attached to (rendered purple, never orange). */
fun isSessionAttached(d: DiscoveredInfo, attachedId: String): Boolean =
    d.registered && attachedId.isNotEmpty() && d.sessionId == attachedId

/**
 * True when a session should render in [BuddyOrange]: it's thinking now, or it's holding output
 * you haven't seen. The attached session is excluded — you're already looking at it.
 */
fun needsAttention(d: DiscoveredInfo, attachedId: String, unread: Set<String>): Boolean =
    !isSessionAttached(d, attachedId) && (d.busy || sessionKey(d) in unread)

/**
 * The set of session keys holding unread output.
 *
 * Tracks per-session "activity we've already surfaced", keyed by [sessionKey]. Seeded to a
 * session's current `lastActive` the first time we see it (so nothing is falsely unread on first
 * load) and kept current for the session you're attached to. A session only becomes unread when
 * new output lands for it while you're attached elsewhere. In-memory: a fresh launch starts
 * everyone clean.
 */
@Composable
fun rememberUnreadSessions(discovered: List<DiscoveredInfo>, attachedId: String): Set<String> {
    val seen = remember { mutableStateMapOf<String, Long>() }
    LaunchedEffect(discovered, attachedId) {
        discovered.forEach { d ->
            val id = sessionKey(d)
            val prev = seen[id]
            if (prev == null || id == attachedId) seen[id] = maxOf(prev ?: 0L, d.lastActive)
        }
    }
    return discovered.mapNotNull { d ->
        val id = sessionKey(d)
        val mark = seen[id]
        if (d.sessionId != attachedId && mark != null && d.lastActive > mark) id else null
    }.toSet()
}

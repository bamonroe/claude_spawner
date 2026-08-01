package com.bam.spawner

import com.bam.spawner.net.TokenUsage
import com.bam.spawner.net.TurnStats

/** Who a chat message is from — drives left/right alignment + colour in the UI. */
enum class Role { USER, CLAUDE, SYSTEM }

/** Hands-free pipeline state, surfaced as a status pill in the UI. */
enum class VoiceState { OFF, LISTENING, CAPTURING, TRANSCRIBING, THINKING, SPEAKING }

/** One line in the chat log. `id` is the row's durable backend identity (stable
 *  across clear/compress rotation; empty for live rows and id-less backends);
 *  `index` ties a live row back to its server-history slot positionally;
 *  `usage` carries the per-turn token badge; `turnStats` the agentic-loop rollup
 *  (cycle count + aggregate) shown in the detailed badge; `ts` is unix seconds (0
 *  for history). */
data class ChatMessage(
    val role: Role,
    val text: String,
    val index: Int = -1,
    val usage: TokenUsage? = null,
    val ts: Long = 0L,
    val id: String = "",
    val turnStats: TurnStats? = null,
)

/**
 * Order a chat log chronologically by timestamp so a live (index < 0) row —
 * a system ack ("cleared, starting fresh"), a mid-turn breadcrumb, a still-streaming
 * reply — lands in its true slot instead of being stranded at the bottom. History
 * carries server transcript time; live rows carry the phone/browser wall clock at
 * arrival (addChat); both are unix seconds, so they interleave correctly.
 *
 * This is the shared ordering both platforms merge through: the web used to sort by
 * transcript `index` alone (live rows → Int.MAX_VALUE → dumped below the whole
 * transcript, so every old control ack re-popped at the bottom on each reattach),
 * while Android already ordered by ts. Unifying on ts removes that divergence.
 *
 * Two guards keep it safe: (1) rows predating transcript timestamps have ts==0, so a
 * timestamp is carried forward from the preceding row (computed on the pre-sort order,
 * where the zeros sit contiguously at the front of the history block) instead of
 * letting them float to the top — callers pass history in index order for this to
 * hold; (2) the sort is stable, so equal timestamps preserve the existing order
 * (history ahead of the live tail, live in arrival order).
 */
fun orderByTimestamp(msgs: List<ChatMessage>): List<ChatMessage> {
    var carried = 0L
    val stamped = msgs.map { m ->
        if (m.ts > 0L) carried = m.ts
        m to carried
    }
    return stamped.sortedBy { it.second }.map { it.first }
}

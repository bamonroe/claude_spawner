package com.bam.spawner

import com.bam.spawner.net.TokenUsage

/** Who a chat message is from — drives left/right alignment + colour in the UI. */
enum class Role { USER, CLAUDE, SYSTEM }

/** One line in the chat log. `index` ties a live row back to its server-history slot;
 *  `usage` carries the per-turn token badge; `ts` is unix seconds (0 for history). */
data class ChatMessage(
    val role: Role,
    val text: String,
    val index: Int = -1,
    val usage: TokenUsage? = null,
    val ts: Long = 0L,
)

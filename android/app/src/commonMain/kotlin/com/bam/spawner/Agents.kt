package com.bam.spawner

import com.bam.spawner.net.AgentInfo

/**
 * Fallback AI-backend id, used only until the server's `agents` registry arrives
 * (or on a pre-agent server that never sends it). The server's default backend is
 * Claude today, so this matches it; once the registry lands, [defaultAgentId]
 * prefers the server-advertised order and this constant is irrelevant.
 */
const val FALLBACK_AGENT_ID = "claude"

/**
 * The server's default AI backend id: the first entry of the advertised `agents`
 * registry (the server lists the default backend first), falling back to
 * [FALLBACK_AGENT_ID] until the registry arrives. UI code that needs "the default
 * agent" resolves it through this — never a hardcoded backend-id literal.
 */
fun defaultAgentId(agents: List<AgentInfo>): String = agents.firstOrNull()?.id ?: FALLBACK_AGENT_ID

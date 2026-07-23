package com.bam.spawner

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.Button
import androidx.compose.material3.FilterChip
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.AgentInfo
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice the shared Providers editor (Settings → Providers) needs. The AI
 * backends are compile-time; the app only edits per-backend overrides — the model
 * a fresh spawn defaults to, and which models the voice commands enumerate. Both
 * ride on the server-broadcast `agents` list; a `provider_put` persists a change.
 */
interface ProvidersController {
    val connected: StateFlow<Boolean>
    val agents: StateFlow<List<AgentInfo>>
    fun putProvider(agent: String, defaultModel: String, voiceModels: List<String>)
}

/**
 * Settings → Providers. One card per AI backend: pick the model a fresh spawn
 * defaults to, and toggle which models the voice "list models" / "use model N"
 * commands enumerate. The backends and their catalogues are fixed server-side; a
 * Save writes a `provider_put` and the server re-broadcasts the enriched `agents`.
 */
@Composable
fun ProvidersSettings(controller: ProvidersController, onBack: () -> Unit) {
    val agents by controller.agents.collectAsState()
    val connected by controller.connected.collectAsState()

    SettingsScaffold("Providers", onBack) {
        Text(
            "Providers are the AI backends the server can run (Claude, Codex, opencode). The backends "
                + "and their model lists are fixed on the server — here you choose, per backend, the model "
                + "a new session starts on, and which models the voice “list models” / “use model N” "
                + "commands read out. Hiding a model from voice keeps it in the visual picker.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage providers.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }
        if (agents.isEmpty()) {
            Text("No backends advertised yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
        }
        for (a in agents) {
            HorizontalDivider()
            ProviderCard(a, connected, controller::putProvider)
        }
    }
}

/** One backend's editable settings card (default model + voice-enumerable set). */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun ProviderCard(
    agent: AgentInfo,
    connected: Boolean,
    onSave: (String, String, List<String>) -> Unit,
) {
    // Local edit state, seeded from the server and re-seeded whenever a broadcast
    // changes this backend's saved values (e.g. right after our own Save).
    var selDefault by remember(agent.id) { mutableStateOf(agent.defaultModel) }
    var voiceSel by remember(agent.id) { mutableStateOf(agent.voiceModels.toSet()) }
    LaunchedEffect(agent.defaultModel, agent.voiceModels) {
        selDefault = agent.defaultModel
        voiceSel = agent.voiceModels.toSet()
    }
    val dirty = selDefault != agent.defaultModel || voiceSel != agent.voiceModels.toSet()

    Surface(
        color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
        shape = RoundedCornerShape(12.dp),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Column(Modifier.padding(14.dp)) {
            Text(agent.name, style = MaterialTheme.typography.titleMedium)
            Text("${agent.id}  ·  ${agent.models.size} models", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)

            if (agent.models.isEmpty()) {
                Text("No selectable models.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
                return@Column
            }

            Text("Default model", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                for (m in agent.models) {
                    FilterChip(selected = selDefault == m, onClick = { selDefault = m }, label = { Text(m) })
                }
            }

            Text("Enumerated by voice", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                for (m in agent.models) {
                    val on = m in voiceSel
                    FilterChip(
                        selected = on,
                        onClick = { voiceSel = if (on) voiceSel - m else voiceSel + m },
                        label = { Text(m) },
                    )
                }
            }

            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && dirty,
                    onClick = {
                        // Send the voice set in catalogue order so the spoken ordinals are stable.
                        onSave(agent.id, selDefault, agent.models.filter { it in voiceSel })
                    },
                ) { Text("Save") }
                if (dirty) {
                    OutlinedButton(onClick = { selDefault = agent.defaultModel; voiceSel = agent.voiceModels.toSet() }) { Text("Reset") }
                }
            }
        }
    }
}

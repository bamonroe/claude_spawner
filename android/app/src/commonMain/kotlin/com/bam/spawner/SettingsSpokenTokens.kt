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
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import com.bam.spawner.net.ActionInfo
import com.bam.spawner.net.SpokenTokenInfo
import kotlinx.coroutines.flow.StateFlow

/**
 * The slice the shared Spoken-tokens editor (Settings → Spoken tokens) needs. The
 * app-managed catalogue binds spoken phrases to a closed set of server-advertised
 * actions (wake/end/speech-gate), with an optional dedicated-detector model. Both
 * clients show and edit the same server-persisted list; the server broadcasts an
 * updated `spoken_tokens` after every change and advertises `actions` on connect.
 */
interface SpokenTokensController {
    val connected: StateFlow<Boolean>
    val spokenTokens: StateFlow<List<SpokenTokenInfo>>
    val spokenActions: StateFlow<List<ActionInfo>>
    fun putSpokenToken(t: SpokenTokenInfo)
    fun deleteSpokenToken(name: String)
}

/**
 * Settings → Spoken tokens. The app-managed catalogue of spoken phrases that trigger
 * the app's spoken features: the **wake** word that opens a command, the **end** token
 * that commits a hands-free message, and the **speech gate**. Several phrases can
 * trigger the same action (so "hey buddy" and "hey gecko" both wake), and a phrase
 * can carry a dedicated-detector (ONNX) model that scores it when the wake-word
 * service is on. This list REPLACES the old built-in wake word; the server persists
 * it and broadcasts an updated `spoken_tokens` after every change, advertising the
 * bindable actions as `actions` on connect.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun SpokenTokensSettings(controller: SpokenTokensController, onBack: () -> Unit) {
    val tokens by controller.spokenTokens.collectAsState()
    val advertised by controller.spokenActions.collectAsState()
    val connected by controller.connected.collectAsState()

    // Fall back to the three built-in actions if the server hasn't advertised yet
    // (older server), so the editor still works.
    val actionList = if (advertised.isNotEmpty()) advertised else listOf(
        ActionInfo("wake", "Wake", "Starts a spoken command, e.g. \"hey buddy\"."),
        ActionInfo("end", "End", "Commits a hands-free message, e.g. \"beep\"."),
        ActionInfo("speech_gate", "Speech gate", "Opens the dictation gate; only speech after it is dictated."),
    )

    var phrase by rememberSaveable { mutableStateOf("") }
    var action by rememberSaveable { mutableStateOf(actionList.first().id) }
    var model by rememberSaveable { mutableStateOf("") }
    var editing by rememberSaveable { mutableStateOf("") } // token name being edited, "" = new
    var showForm by rememberSaveable { mutableStateOf(false) }
    val clear = { phrase = ""; action = actionList.first().id; model = ""; editing = "" }

    SettingsScaffold("Spoken tokens", onBack) {
        Text(
            "Spoken tokens are the phrases that trigger the app's spoken features. Several phrases can "
                + "trigger the same action — so \"hey buddy\" and \"hey gecko\" can both wake. A phrase with a "
                + "detector model is scored by that model when the wake-word service is on; otherwise it's "
                + "matched in the Whisper transcript. The app owns this list; the server shares it across devices.",
            style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
        )
        if (!connected) {
            Text("Connect to the server to manage spoken tokens.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
        }

        for (act in actionList) {
            val group = tokens.filter { it.action == act.id }
            HorizontalDivider()
            Text(act.label, style = MaterialTheme.typography.titleMedium)
            if (act.desc.isNotBlank()) {
                Text(act.desc, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            if (group.isEmpty()) {
                Text("None yet.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            }
            for (t in group) {
                Surface(
                    color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.4f),
                    shape = RoundedCornerShape(12.dp),
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Row(Modifier.padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text(t.phrase, style = MaterialTheme.typography.titleMedium)
                            Text(
                                if (t.model.isNotBlank()) "detector model: ${t.model}" else "Whisper match",
                                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline,
                            )
                        }
                        TextButton(onClick = {
                            phrase = t.phrase; action = t.action; model = t.model; editing = t.name; showForm = true
                        }) { Text("Edit") }
                        TextButton(onClick = {
                            controller.deleteSpokenToken(t.name)
                            if (editing == t.name) { clear(); showForm = false }
                        }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                    }
                }
            }
        }

        HorizontalDivider()
        if (!showForm && editing.isBlank()) {
            Button(enabled = connected, onClick = { clear(); showForm = true }) { Text("Add token") }
        } else {
            Text(if (editing.isBlank()) "Add token" else "Editing “$editing”", style = MaterialTheme.typography.titleMedium)
            OutlinedTextField(phrase, { phrase = it }, label = { Text("Phrase (e.g. hey buddy)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Text("Action", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.outline)
            FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                for (a in actionList) {
                    FilterChip(selected = action == a.id, onClick = { action = a.id }, label = { Text(a.label) })
                }
            }
            OutlinedTextField(model, { model = it }, label = { Text("Detector model (blank = Whisper match)") }, singleLine = true, modifier = Modifier.fillMaxWidth())
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Button(
                    enabled = connected && phrase.isNotBlank(),
                    onClick = {
                        val recordName = if (editing.isNotBlank()) editing else "tok-${nowEpochMs()}"
                        controller.putSpokenToken(
                            SpokenTokenInfo(name = recordName, phrase = phrase.trim(), action = action, model = model.trim()),
                        )
                        clear(); showForm = false
                    },
                ) { Text(if (editing.isBlank()) "Add" else "Save") }
                OutlinedButton(onClick = { clear(); showForm = false }) { Text("Cancel") }
            }
        }
    }
}

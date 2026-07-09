package com.bam.spawner

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.AttachFile
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.unit.dp
import kotlin.js.JsString
import kotlin.js.Promise

// ── Browser file I/O ────────────────────────────────────────────────────────
// The transfer protocol carries bytes as base64 in one JSON frame (see docs/protocol.md),
// so the only platform work is reading a picked file into base64 and writing a downloaded
// blob back out. Both are done in plain JS: the DOM File API to read, an object-URL anchor
// to save. Kept out of the composable so the UI below is pure Compose.

/** Opens the browser file picker and, once a file is chosen, calls [onPicked] with its
 *  name and base64 content. The picker is opened synchronously inside the click handler
 *  (browsers require the `<input>.click()` to be in the user-gesture task); the result is
 *  delivered later via the FileReader promise. */
fun pickFileB64(onPicked: (name: String, contentB64: String) -> Unit) {
    jsPickFileB64().then<JsAny?> { packed: JsString ->
        val s = packed.toString()
        if (s.isNotEmpty()) {
            val nl = s.indexOf('\n')
            if (nl >= 0) onPicked(s.substring(0, nl), s.substring(nl + 1))
        }
        null
    }
}

// The DOM result is packed as "<filename>\n<base64>" (empty string if the user cancelled),
// so a single JsString crosses the boundary. readAsDataURL yields "data:...;base64,XXXX";
// we keep only the part after the comma.
private fun jsPickFileB64(): Promise<JsString> = js(
    """
    new Promise(function(resolve){
      var input = document.createElement('input');
      input.type = 'file';
      input.onchange = function(){
        var f = input.files && input.files[0];
        if(!f){ resolve(''); return; }
        var r = new FileReader();
        r.onload = function(){
          var s = String(r.result);
          var comma = s.indexOf(',');
          resolve(f.name + '\n' + (comma >= 0 ? s.substring(comma+1) : s));
        };
        r.onerror = function(){ resolve(''); };
        r.readAsDataURL(f);
      };
      input.click();
    })
    """,
)

/** Saves base64 [b64] to the user's downloads as [name], via a temporary blob URL. */
fun saveB64(name: String, b64: String): Unit = js(
    """
    {
      var bin = atob(b64);
      var bytes = new Uint8Array(bin.length);
      for (var i = 0; i < bin.length; i++) bytes[i] = bin.charCodeAt(i);
      var url = URL.createObjectURL(new Blob([bytes], {type: 'application/octet-stream'}));
      var a = document.createElement('a');
      a.href = url; a.download = name;
      document.body.appendChild(a); a.click(); document.body.removeChild(a);
      setTimeout(function(){ URL.revokeObjectURL(url); }, 1000);
    }
    """,
)

// ── The web 📎 button ────────────────────────────────────────────────────────

/** The browser equivalent of the Android SAF transfer button: a menu to upload a
 *  browser-picked file onto the session's host, or download a host file back to the
 *  browser's downloads — both over the same authenticated WebSocket, reusing the shared
 *  [TransferPickerDialog] to choose the host directory/file. An upload prefills the
 *  message box via [onUploaded]. */
@Composable
fun WebTransferButton(
    controller: AppController,
    enabled: Boolean,
    onUploaded: (String) -> Unit,
) {
    var menu by remember { mutableStateOf(false) }
    // Non-null while the destination-directory picker is open for an upload.
    var pendingUpload by remember { mutableStateOf<PendingWebUpload?>(null) }
    // Non-null while the download file-picker is open (its browse start point).
    var downloadStart by remember { mutableStateOf<DirHost?>(null) }

    fun start(): DirHost = controller.attachedDirHost()?.let { DirHost(it.first, it.second) } ?: DirHost("/", "")

    // An upload landed on the host: prefill the message box (do NOT send).
    LaunchedEffect(Unit) { controller.fileSaved.collect { onUploaded(it) } }
    // A download's bytes arrived: hand them to the browser as a file save.
    LaunchedEffect(Unit) { controller.fileData.collect { fd -> saveB64(fd.name, fd.content) } }

    Box {
        Box(
            Modifier.size(48.dp).clip(CircleShape)
                .background(MaterialTheme.colorScheme.surfaceVariant)
                .clickable(enabled = enabled) { menu = true },
            contentAlignment = Alignment.Center,
        ) { Icon(Icons.Filled.AttachFile, contentDescription = "Transfer a file") }
        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
            DropdownMenuItem(text = { Text("Upload file") }, onClick = {
                menu = false
                val where = start()
                pickFileB64 { name, b64 -> pendingUpload = PendingWebUpload(name, b64, where) }
            })
            DropdownMenuItem(text = { Text("Download file") }, onClick = {
                menu = false; downloadStart = start()
            })
        }
    }

    // Upload: choose the destination directory on the session's host, then send.
    pendingUpload?.let { up ->
        TransferPickerDialog(
            controller = controller,
            host = up.start.host,
            startDir = up.start.dir,
            pickFiles = false,
            title = "Upload “${up.name}” to…",
            onPick = { dir ->
                controller.uploadFile(dir, up.name, up.content, up.start.host)
                pendingUpload = null
            },
            onDismiss = { pendingUpload = null },
        )
    }

    // Download: choose a file on the session's host; its bytes come back as file_data.
    downloadStart?.let { s ->
        TransferPickerDialog(
            controller = controller,
            host = s.host,
            startDir = s.dir,
            pickFiles = true,
            title = "Download a file",
            onPick = { path ->
                controller.downloadFile(path, s.host)
                downloadStart = null
            },
            onDismiss = { downloadStart = null },
        )
    }
}

/** A browser file the user picked to upload, held while they choose a destination dir. */
private data class PendingWebUpload(val name: String, val content: String, val start: DirHost)

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

/** A browser file the user picked to upload: its display name and base64 bytes. */
data class WebUploadFile(val name: String, val content: String)

/** Opens the browser file picker (multi-select enabled) and, once files are chosen, calls
 *  [onPicked] with every picked file's name and base64 content. The picker is opened
 *  synchronously inside the click handler (browsers require the `<input>.click()` to be in
 *  the user-gesture task); the result is delivered later via the FileReader promise. */
fun pickFilesB64(onPicked: (List<WebUploadFile>) -> Unit) {
    jsPickFilesB64().then<JsAny?> { packed: JsString ->
        // The result is a flat list of alternating name / base64 lines (see below), so
        // walk it two lines at a time. Neither a filename nor single-line base64 can
        // contain a newline, so this pairing is unambiguous.
        val lines = packed.toString().split('\n')
        val files = mutableListOf<WebUploadFile>()
        var i = 0
        while (i + 1 < lines.size) { files.add(WebUploadFile(lines[i], lines[i + 1])); i += 2 }
        if (files.isNotEmpty()) onPicked(files)
        null
    }
}

// Every chosen file is packed as two newline-separated lines — "<name>" then "<base64>" —
// and all files are concatenated into one newline-joined JsString (empty if the user
// cancelled), so a single value crosses the boundary. readAsDataURL yields
// "data:...;base64,XXXX"; we keep only the part after the comma.
private fun jsPickFilesB64(): Promise<JsString> = js(
    """
    new Promise(function(resolve){
      var input = document.createElement('input');
      input.type = 'file';
      input.multiple = true;
      input.onchange = function(){
        var files = input.files;
        if(!files || !files.length){ resolve(''); return; }
        var out = new Array(files.length);
        var done = 0;
        for(var i = 0; i < files.length; i++){
          (function(idx){
            var f = files[idx];
            var r = new FileReader();
            r.onload = function(){
              var s = String(r.result);
              var comma = s.indexOf(',');
              out[idx] = f.name + '\n' + (comma >= 0 ? s.substring(comma+1) : s);
              if(++done === files.length) resolve(out.filter(function(x){return x;}).join('\n'));
            };
            r.onerror = function(){
              out[idx] = '';
              if(++done === files.length) resolve(out.filter(function(x){return x;}).join('\n'));
            };
            r.readAsDataURL(f);
          })(i);
        }
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

/** The browser equivalent of the Android SAF transfer button: a menu to upload one or
 *  more browser-picked files onto the session's host, or download one or more host files
 *  back to the browser's downloads — both over the same authenticated WebSocket, reusing
 *  the shared [TransferPickerDialog] to choose the host directory/files. An upload prefills
 *  the message box via [onUploaded]. */
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
    // A download's bytes arrived: hand them to the browser as a file save (the browser
    // happily runs several concurrent blob downloads, so no queueing is needed here).
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
                pickFilesB64 { files -> pendingUpload = PendingWebUpload(files, where) }
            })
            DropdownMenuItem(text = { Text("Download file") }, onClick = {
                menu = false; downloadStart = start()
            })
        }
    }

    // Upload: choose the destination directory on the session's host, then send each file.
    pendingUpload?.let { up ->
        val title = up.files.singleOrNull()?.let { "Upload “${it.name}” to…" }
            ?: "Upload ${up.files.size} files to…"
        TransferPickerDialog(
            controller = controller,
            host = up.start.host,
            startDir = up.start.dir,
            pickFiles = false,
            title = title,
            onPick = { dirs ->
                val dir = dirs.first()
                up.files.forEach { controller.uploadFile(dir, it.name, it.content, up.start.host) }
                pendingUpload = null
            },
            onDismiss = { pendingUpload = null },
        )
    }

    // Download: tick one or more files on the session's host; each comes back as file_data.
    downloadStart?.let { s ->
        TransferPickerDialog(
            controller = controller,
            host = s.host,
            startDir = s.dir,
            pickFiles = true,
            title = "Download files",
            onPick = { paths ->
                paths.forEach { controller.downloadFile(it, s.host) }
                downloadStart = null
            },
            onDismiss = { downloadStart = null },
        )
    }
}

/** Browser files the user picked to upload, held while they choose a destination dir. */
private data class PendingWebUpload(val files: List<WebUploadFile>, val start: DirHost)

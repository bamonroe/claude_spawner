package com.bam.spawner.tts

import android.content.Context
import android.util.Log
import java.io.File
import java.io.InputStream
import java.net.HttpURLConnection
import java.net.URL
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import org.apache.commons.compress.archivers.tar.TarArchiveInputStream
import org.apache.commons.compress.compressors.bzip2.BZip2CompressorInputStream

/**
 * Downloads, unpacks and owns the on-device TTS model bundles.
 *
 * Models live under `filesDir/tts/<model id>/` and are far too big to ship in
 * the APK (Kokoro int8 is ~103 MB), so they're fetched on demand from the
 * k2-fsa release and unpacked in one streaming pass — the `.tar.bz2` is
 * decompressed straight from the socket, never landing on disk as an archive.
 *
 * A model is "installed" iff its directory holds a marker file written *after*
 * the unpack finished. That makes a killed download self-healing: a partial
 * directory has no marker, so it's discarded and refetched rather than loaded
 * as a truncated model.
 */
class TtsModelStore(context: Context, private val scope: CoroutineScope) {
    private val root = File(context.filesDir, "tts")

    private val _states = MutableStateFlow<Map<String, TtsModelState>>(emptyMap())
    val states: StateFlow<Map<String, TtsModelState>> = _states.asStateFlow()

    private val jobs = mutableMapOf<String, Job>()

    init {
        _states.value = TtsCatalogue.ALL.associate {
            it.id to TtsModelState(it.id, installed = isInstalled(it.id))
        }
    }

    /** The unpacked directory for [id], or null when it isn't installed. */
    fun dirFor(id: String): File? = if (isInstalled(id)) modelDir(id) else null

    fun isInstalled(id: String): Boolean = File(modelDir(id), MARKER).isFile

    private fun modelDir(id: String) = File(root, id)

    private fun update(id: String, f: (TtsModelState) -> TtsModelState) {
        _states.value = _states.value.toMutableMap().apply {
            this[id] = f(this[id] ?: TtsModelState(id))
        }
    }

    /** Fetch and unpack [id]. No-op when it's already installed or downloading. */
    fun install(id: String) {
        val model = TtsCatalogue.byId(id) ?: return
        synchronized(jobs) {
            if (jobs[id]?.isActive == true || isInstalled(id)) return
            jobs[id] = scope.launch(Dispatchers.IO) { download(model) }
        }
    }

    /** Delete [id]'s files. Cancels an in-flight download for it first. */
    fun remove(id: String) {
        synchronized(jobs) { jobs.remove(id)?.cancel() }
        modelDir(id).deleteRecursively()
        update(id) { TtsModelState(id, installed = false) }
    }

    private fun download(model: TtsModel) {
        val dir = modelDir(model.id)
        update(model.id) { it.copy(downloading = true, received = 0, total = model.bytes, error = "") }
        // Always start from a clean directory: a leftover partial unpack would
        // otherwise mix files from two attempts.
        dir.deleteRecursively()
        dir.mkdirs()
        try {
            val conn = (URL(model.url).openConnection() as HttpURLConnection).apply {
                instanceFollowRedirects = true
                connectTimeout = 30_000
                readTimeout = 60_000
            }
            conn.inputStream.use { raw ->
                val total = conn.contentLengthLong.takeIf { it > 0 } ?: model.bytes
                update(model.id) { it.copy(total = total) }
                val counted = ProgressStream(raw) { n ->
                    update(model.id) { it.copy(received = n) }
                }
                unpack(counted, dir)
            }
            if (!scope.isActive) return
            File(dir, MARKER).writeText(model.url)
            update(model.id) { it.copy(installed = true, downloading = false, received = it.total) }
        } catch (e: Exception) {
            Log.w(TAG, "install ${model.id} failed", e)
            dir.deleteRecursively()
            update(model.id) {
                it.copy(installed = false, downloading = false, received = 0,
                    error = e.message ?: e::class.java.simpleName)
            }
        }
    }

    /**
     * Stream the tar.bz2 into [dir], flattening the archive's single top-level
     * directory away so every bundle unpacks to the same shape regardless of what
     * upstream named it.
     */
    private fun unpack(input: InputStream, dir: File) {
        TarArchiveInputStream(BZip2CompressorInputStream(input, true)).use { tar ->
            while (true) {
                val entry = tar.nextEntry ?: break
                // Strip the leading "<bundle-name>/" component.
                val rel = entry.name.substringAfter('/', "")
                if (rel.isEmpty()) continue
                val out = File(dir, rel)
                // Refuse anything that would escape the model directory (a tar
                // with ../ entries); we don't control the archive's contents.
                if (!out.canonicalPath.startsWith(dir.canonicalPath + File.separator)) continue
                if (entry.isDirectory) {
                    out.mkdirs()
                } else {
                    out.parentFile?.mkdirs()
                    out.outputStream().use { tar.copyTo(it) }
                }
            }
        }
    }

    /** Wraps a stream to report cumulative bytes read, throttled to whole percents. */
    private class ProgressStream(
        private val inner: InputStream,
        private val onProgress: (Long) -> Unit,
    ) : InputStream() {
        private var count = 0L
        private var lastReport = 0L

        override fun read(): Int = inner.read().also { if (it >= 0) bump(1) }

        override fun read(b: ByteArray, off: Int, len: Int): Int =
            inner.read(b, off, len).also { if (it > 0) bump(it.toLong()) }

        override fun close() = inner.close()

        private fun bump(n: Long) {
            count += n
            if (count - lastReport >= REPORT_EVERY) {
                lastReport = count
                onProgress(count)
            }
        }
    }

    private companion object {
        const val TAG = "TtsModelStore"
        // Written only once the unpack completed — its presence *is* "installed".
        const val MARKER = ".installed"
        const val REPORT_EVERY = 512L * 1024
    }
}

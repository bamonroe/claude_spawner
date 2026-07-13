package com.bam.spawner

import kotlin.js.JsString
import kotlin.js.Promise

// ── Browser microphone + speech ──────────────────────────────────────────────
// The mic path mirrors the phone's push-to-talk: capture the whole clip, then send it
// on release (see docs/protocol.md — `wake` → binary audio → `audio_end`). Rather than
// stream frames (which would need a JS→Wasm callback per buffer), we accumulate the
// Float32 samples in JS for the duration of the press and, on stop, downsample to
// 16 kHz mono PCM16LE and hand the whole clip back as base64 in one boundary crossing.
// The server's `pcm16` codec wraps it in a WAV header for whisper — no ffmpeg/Opus
// needed. Speech-out uses the browser's built-in SpeechSynthesis. All of it is plain JS
// via `js(...)`, matching the file-transfer helpers in WebTransfer.kt.

/**
 * Ask for the microphone and start accumulating audio. Resolves to `"1"` once capture is
 * running, or `"err:<reason>"` if permission was denied or no device is available. The mic
 * is stored on a `window` global so [stopMic]/[cancelMic] can find it; calling twice while
 * already active is a no-op that resolves `"1"`.
 */
fun startMic(): Promise<JsString> = js(
    """
    (function(){
      var M = window.__spawnerMic = window.__spawnerMic || {};
      if (M.active) return Promise.resolve('1');
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia)
        return Promise.resolve('err:no-mic-api');
      return navigator.mediaDevices.getUserMedia({audio:{channelCount:1, echoCancellation:true, noiseSuppression:true, autoGainControl:true}})
        .then(function(stream){
          var Ctx = window.AudioContext || window.webkitAudioContext;
          var ctx = new Ctx();
          var src = ctx.createMediaStreamSource(stream);
          var proc = ctx.createScriptProcessor(4096, 1, 1);
          M.chunks = [];
          M.rate = ctx.sampleRate;
          proc.onaudioprocess = function(e){
            if (!M.active) return;
            M.chunks.push(new Float32Array(e.inputBuffer.getChannelData(0)));
          };
          src.connect(proc); proc.connect(ctx.destination);
          M.stream = stream; M.ctx = ctx; M.src = src; M.proc = proc; M.active = true;
          return '1';
        })
        .catch(function(err){ return 'err:' + (err && err.name ? err.name : err); });
    })()
    """,
)

/**
 * Stop capture and return the recorded clip as base64 PCM16LE at 16 kHz mono (empty string
 * if nothing was captured, e.g. the button was released before permission resolved).
 */
fun stopMic(): JsString = js(
    """
    (function(){
      var M = window.__spawnerMic;
      if (!M || !M.active) return '';
      M.active = false;
      try { M.proc.disconnect(); M.src.disconnect(); } catch(e){}
      try { M.stream.getTracks().forEach(function(t){ t.stop(); }); } catch(e){}
      try { M.ctx.close(); } catch(e){}
      var total = 0, i;
      for (i = 0; i < M.chunks.length; i++) total += M.chunks[i].length;
      var flat = new Float32Array(total), off = 0;
      for (i = 0; i < M.chunks.length; i++){ flat.set(M.chunks[i], off); off += M.chunks[i].length; }
      M.chunks = [];
      var ratio = M.rate / 16000;
      var outLen = Math.floor(flat.length / ratio);
      var out = new Int16Array(outLen);
      for (var j = 0; j < outLen; j++){
        var s = flat[Math.floor(j * ratio)];
        s = Math.max(-1, Math.min(1, s));
        out[j] = s < 0 ? s * 0x8000 : s * 0x7FFF;
      }
      var bytes = new Uint8Array(out.buffer), bin = '', CH = 0x8000;
      for (var k = 0; k < bytes.length; k += CH)
        bin += String.fromCharCode.apply(null, bytes.subarray(k, k + CH));
      return btoa(bin);
    })()
    """,
)

/** Abandon an in-progress capture (swipe-cancel): tear down the mic, discard the buffer. */
fun cancelMic(): Unit = js(
    """
    {
      var M = window.__spawnerMic;
      if (M && M.active) {
        M.active = false;
        try { M.proc.disconnect(); M.src.disconnect(); } catch(e){}
        try { M.stream.getTracks().forEach(function(t){ t.stop(); }); } catch(e){}
        try { M.ctx.close(); } catch(e){}
        M.chunks = [];
      }
    }
    """,
)

// ── Hands-free (VAD-gated always-listening) ──────────────────────────────────
// The browser analogue of the phone's HandsFreeRecorder + Endpointer: one open mic,
// an energy VAD that segments each utterance (onset → silence), and one pcm16 clip
// per utterance handed back the same way push-to-talk clips are (wake → audio →
// audio_end, with hands_free=true so the server streaming-appends until the end
// token). RMS is scaled to the int16 range so the shared [Prefs.vadThreshold] (default
// 500) means the same thing here as on Android. While SpeechSynthesis is speaking the
// bar is tripled to reject TTS echo leaking into the mic (getUserMedia's own
// echoCancellation handles the rest). Finished clips queue on a `window` global that
// Kotlin drains by polling (avoids a JS→Wasm callback per utterance).

/**
 * Open the mic and start VAD-gated always-listening. [thresholdRms] is the int16-scale RMS
 * bar; [onsetMs] of sustained voice starts an utterance; [silenceMs] of quiet ends it;
 * [maxMs] hard-caps one utterance. Resolves `"1"` once running, or `"err:<reason>"`.
 */
fun startHandsFreeMic(thresholdRms: Int, onsetMs: Int, silenceMs: Int, maxMs: Int): Promise<JsString> = js(
    """
    (function(){
      var H = window.__spawnerHF = window.__spawnerHF || {};
      if (H.active) return Promise.resolve('1');
      if (!navigator.mediaDevices || !navigator.mediaDevices.getUserMedia)
        return Promise.resolve('err:no-mic-api');
      return navigator.mediaDevices.getUserMedia({audio:{channelCount:1, echoCancellation:true, noiseSuppression:true, autoGainControl:true}})
        .then(function(stream){
          var Ctx = window.AudioContext || window.webkitAudioContext;
          var ctx = new Ctx();
          var src = ctx.createMediaStreamSource(stream);
          var proc = ctx.createScriptProcessor(2048, 1, 1);
          H.thr = thresholdRms; H.onsetMs = onsetMs; H.silenceMs = silenceMs; H.maxMs = maxMs;
          H.rate = ctx.sampleRate;
          H.capturing = false; H.voicedMs = 0; H.silMs = 0; H.uttMs = 0;
          H.pend = []; H.chunks = []; H.ready = [];
          proc.onaudioprocess = function(e){
            if (!H.active) return;
            var d = e.inputBuffer.getChannelData(0);
            var frame = new Float32Array(d); // copy: the buffer is recycled
            var n = frame.length, frameMs = n / H.rate * 1000, i, sum = 0;
            for (i = 0; i < n; i++) sum += frame[i] * frame[i];
            var rms = Math.sqrt(sum / n) * 32768;
            var thr = H.thr;
            // Triple the bar while we're speaking (browser voice OR server-voice
            // playback), so our own TTS doesn't self-trigger.
            if ((window.speechSynthesis && window.speechSynthesis.speaking) ||
                (window.__spawnerTts && window.__spawnerTts.playing)) thr = thr * 3;
            var voiced = rms >= thr;
            if (!H.capturing) {
              if (voiced) {
                H.voicedMs += frameMs; H.pend.push(frame);
                if (H.voicedMs >= H.onsetMs) {
                  // Onset confirmed: keep the buffered pre-roll so the first word isn't clipped.
                  H.capturing = true; H.chunks = H.pend; H.pend = [];
                  H.silMs = 0; H.uttMs = H.voicedMs;
                }
              } else { H.voicedMs = 0; H.pend = []; } // a noise blip — drop it
            } else {
              H.chunks.push(frame); H.uttMs += frameMs;
              if (voiced) H.silMs = 0; else H.silMs += frameMs;
              if (H.silMs >= H.silenceMs || H.uttMs >= H.maxMs) {
                // Finalize: flatten, downsample to 16 kHz mono PCM16LE, base64, queue it.
                var total = 0, j;
                for (j = 0; j < H.chunks.length; j++) total += H.chunks[j].length;
                var flat = new Float32Array(total), off = 0;
                for (j = 0; j < H.chunks.length; j++){ flat.set(H.chunks[j], off); off += H.chunks[j].length; }
                var ratio = H.rate / 16000, outLen = Math.floor(flat.length / ratio);
                var out = new Int16Array(outLen), s;
                for (var k = 0; k < outLen; k++){ s = flat[Math.floor(k * ratio)]; s = Math.max(-1, Math.min(1, s)); out[k] = s < 0 ? s * 0x8000 : s * 0x7FFF; }
                var bytes = new Uint8Array(out.buffer), bin = '', CH = 0x8000;
                for (var m = 0; m < bytes.length; m += CH) bin += String.fromCharCode.apply(null, bytes.subarray(m, m + CH));
                if (outLen > 0) H.ready.push(btoa(bin));
                H.capturing = false; H.voicedMs = 0; H.silMs = 0; H.uttMs = 0; H.pend = []; H.chunks = [];
              }
            }
          };
          src.connect(proc); proc.connect(ctx.destination);
          H.stream = stream; H.ctx = ctx; H.src = src; H.proc = proc; H.active = true;
          return '1';
        })
        .catch(function(err){ return 'err:' + (err && err.name ? err.name : err); });
    })()
    """,
)

/** Dequeue the next finished hands-free clip as base64 PCM16LE (16 kHz mono), or "" if none is ready. */
fun pollHandsFreeClip(): JsString = js(
    """
    (function(){
      var H = window.__spawnerHF;
      if (!H || !H.ready || !H.ready.length) return '';
      return H.ready.shift();
    })()
    """,
)

/** True while a hands-free utterance is being captured (drives the LISTENING↔CAPTURING pill). */
fun handsFreeCapturing(): Boolean = js("(!!(window.__spawnerHF && window.__spawnerHF.active && window.__spawnerHF.capturing))")

/** Stop always-listening and tear the mic down. */
fun stopHandsFreeMic(): Unit = js(
    """
    {
      var H = window.__spawnerHF;
      if (H && H.active) {
        H.active = false;
        try { H.proc.disconnect(); H.src.disconnect(); } catch(e){}
        try { H.stream.getTracks().forEach(function(t){ t.stop(); }); } catch(e){}
        try { H.ctx.close(); } catch(e){}
        H.pend = []; H.chunks = []; H.ready = []; H.capturing = false;
      }
    }
    """,
)

// ── Server-TTS playback (the Kokoro epic, see TODO.md) ───────────────────────
// The server synthesizes speech (the `speak` protocol) and streams the audio back
// as binary WebSocket frames. The web client asks for mp3 — decodeAudioData wants
// one complete compressed clip (unlike Android's frame-by-frame pcm AudioTrack),
// and mp3 decodes in every browser (Safari has no ogg/opus). Frames accumulate on
// a window global; on speak_end the clip is decoded and queued so back-to-back
// utterances play in order without overlapping. Base64 crosses the Wasm→JS
// boundary the same way the mic path does.

/** Start accumulating a new server-voice utterance (speak_audio arrived). */
fun serverSpeakBegin(): Unit = js(
    """
    {
      var T = window.__spawnerTts = window.__spawnerTts || { queue: [], playing: false, src: null };
      T.chunks = [];
    }
    """,
)

/** Append one binary frame (base64) to the utterance being accumulated. */
fun serverSpeakChunk(b64: String): Unit = js(
    """
    {
      var T = window.__spawnerTts;
      if (!T || !T.chunks) return;
      var bin = atob(b64), n = bin.length, bytes = new Uint8Array(n);
      for (var i = 0; i < n; i++) bytes[i] = bin.charCodeAt(i);
      T.chunks.push(bytes);
    }
    """,
)

/** The utterance's stream closed cleanly: decode the whole clip and queue it for
 *  sequential playback (utterances never overlap; one shared AudioContext). */
fun serverSpeakEnd(): Unit = js(
    """
    {
      var T = window.__spawnerTts;
      if (!T || !T.chunks || !T.chunks.length) return;
      var total = 0, i;
      for (i = 0; i < T.chunks.length; i++) total += T.chunks[i].length;
      var flat = new Uint8Array(total), off = 0;
      for (i = 0; i < T.chunks.length; i++){ flat.set(T.chunks[i], off); off += T.chunks[i].length; }
      T.chunks = [];
      try {
        var AC = window.AudioContext || window.webkitAudioContext;
        if (!AC) return;
        if (!window.__spawnerTtsCtx) window.__spawnerTtsCtx = new AC();
        var ctx = window.__spawnerTtsCtx;
        if (ctx.state === 'suspended') ctx.resume();
        var pump = function(){
          if (T.playing || !T.queue.length) return;
          var buf = T.queue.shift();
          var src = ctx.createBufferSource();
          src.buffer = buf;
          src.connect(ctx.destination);
          src.onended = function(){ T.playing = false; T.src = null; pump(); };
          T.playing = true; T.src = src;
          src.start();
        };
        ctx.decodeAudioData(flat.buffer, function(decoded){ T.queue.push(decoded); pump(); }, function(){});
      } catch (e) {}
    }
    """,
)

/** Whether server-voice audio is playing or queued (drives the speaking pill and
 *  the hands-free echo rejection, alongside SpeechSynthesis). */
fun serverSpeechActive(): Boolean = js("(!!(window.__spawnerTts && (window.__spawnerTts.playing || (window.__spawnerTts.queue && window.__spawnerTts.queue.length > 0))))")

/** Halt server-voice playback immediately and drop everything queued or accumulating. */
fun cancelServerSpeechPlayback(): Unit = js(
    """
    {
      var T = window.__spawnerTts;
      if (!T) return;
      T.chunks = []; T.queue = [];
      try { if (T.src) T.src.stop(); } catch (e) {}
      T.playing = false; T.src = null;
    }
    """,
)

/** Queue [text] to be spoken by the browser's SpeechSynthesis (utterances queue naturally). */
fun speakText(text: String): Unit = js(
    """
    {
      try {
        var u = new SpeechSynthesisUtterance(text);
        window.speechSynthesis.speak(u);
      } catch (e) {}
    }
    """,
)

/** Halt any in-progress or queued speech immediately (barge-in / stop button). */
fun cancelSpeech(): Unit = js("{ try { window.speechSynthesis.cancel(); } catch (e) {} }")

/** Whether SpeechSynthesis is currently speaking or has utterances queued. */
fun speechActive(): Boolean = js("(!!window.speechSynthesis && (window.speechSynthesis.speaking || window.speechSynthesis.pending))")

/**
 * A short, soft, warm beep — the web analogue of the app's summary-only "still
 * working…" cue, played in place of speaking an intermediate step. A low sine
 * with a smooth gain ramp (no sharp onset) so it reads as round, not shrill.
 * Reuses one AudioContext on the window.
 */
fun webBeep(): Unit = js(
    """
    {
      try {
        var AC = window.AudioContext || window.webkitAudioContext;
        if (!AC) return;
        if (!window.__spawnerBeepCtx) window.__spawnerBeepCtx = new AC();
        var ctx = window.__spawnerBeepCtx;
        if (ctx.state === 'suspended') ctx.resume();
        var t0 = ctx.currentTime, dur = 0.2, coalesce = 0.26;
        // Coalesce: swallow a beep that lands while the previous tone is still
        // playing, so a burst reads as one "activity" cue rather than a machine-gun.
        // Once a tone has finished, the next message beeps again — steady tones under
        // sustained work, silence when idle.
        var last = window.__spawnerBeepLast || 0;
        if (t0 - last < coalesce) return;
        window.__spawnerBeepLast = t0;
        var osc = ctx.createOscillator();
        osc.type = 'sine'; osc.frequency.value = 420;
        var g = ctx.createGain();
        g.gain.setValueAtTime(0.0001, t0);
        g.gain.exponentialRampToValueAtTime(0.28, t0 + dur * 0.4);
        g.gain.exponentialRampToValueAtTime(0.0001, t0 + dur);
        osc.connect(g); g.connect(ctx.destination);
        osc.start(t0); osc.stop(t0 + dur);
      } catch (e) {}
    }
    """,
)

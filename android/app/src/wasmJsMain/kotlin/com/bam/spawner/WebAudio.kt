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

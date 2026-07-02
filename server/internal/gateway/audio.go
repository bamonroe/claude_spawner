package gateway

import "github.com/bam/claude_spawner/server/internal/transcribe"

// Audio contract for the voice path: the app sends `wake`, then binary PCM16LE
// frames (16 kHz, mono), then `audio_end`. The server assembles one WAV per
// utterance, transcribes it, echoes a `transcript`, and feeds the text through
// the same path as a typed `utterance`.
const (
	audioSampleRate = 16000
	audioChannels   = 1
	// Cap a single utterance to ~120s of audio to bound memory (16k * 2 bytes/s).
	maxAudioBytes = audioSampleRate * 2 * 120
)

// startAudio begins recording an utterance. codec is "ogg_opus" (the app's
// compressed default) or "pcm16" (raw); empty means pcm16. handsFree marks a
// VAD-gated clip so the server can drop background speech (no wake word, outside
// the follow-up window) instead of acting on it.
func (c *conn) startAudio(codec string, handsFree, calibrate bool) {
	if c.transcriber() == nil {
		c.send(msgError("not_implemented", "audio transcription is disabled; send text as 'utterance'"))
		return
	}
	c.collecting = true
	c.gated = handsFree
	c.calibrate = calibrate
	c.audio = c.audio[:0]
	if codec == "" {
		codec = "pcm16"
	}
	c.audioCodec = codec
}

// handleAudioFrame appends a binary PCM frame to the current utterance.
func (c *conn) handleAudioFrame(frame []byte) {
	if !c.collecting {
		return // stray audio outside wake/audio_end; ignore
	}
	if len(c.audio)+len(frame) > maxAudioBytes {
		return // drop excess; endAudio will still transcribe what we have
	}
	c.audio = append(c.audio, frame...)
}

// endAudio finalizes the utterance: transcribe, echo the transcript, dispatch.
func (c *conn) endAudio() {
	if !c.collecting {
		return
	}
	c.collecting = false
	if c.transcriber() == nil {
		c.send(msgError("not_implemented", "audio transcription is disabled"))
		return
	}
	if len(c.audio) == 0 {
		if !c.gated {
			c.send(msgSay("didn't hear anything, bud."))
		}
		return
	}

	// Decode this clip to raw PCM16 (16 kHz mono).
	var pcm []byte
	if c.audioCodec == "ogg_opus" {
		decoded, derr := transcribe.OggOpusToPCM(c.srv.cfg.FfmpegBin, c.audio)
		if derr != nil {
			c.send(msgError("transcribe_failed", derr.Error()))
			return
		}
		pcm = decoded
	} else {
		pcm = append([]byte(nil), c.audio...) // already PCM16LE 16 kHz mono
	}

	// Calibration: transcribe with the fast (detection) model and just report
	// what it heard — this measures exactly what end-token detection sees.
	if c.calibrate {
		text, _ := c.transcriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
			transcribe.Options{Mode: "fixed", Model: "tiny"})
		c.send(msgCalibration(text))
		return
	}

	// Hands-free: keep the audio and only fast-transcribe for the draft + end
	// token — the whole message is re-transcribed accurately on commit.
	if c.gated {
		c.gatedChunk(pcm)
		return
	}

	// Push-to-talk / typed audio: transcribe now with the chosen model, dispatch.
	text, err := c.transcriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
		transcribe.Options{Mode: c.sttMode, Model: c.sttModel})
	if err != nil {
		c.send(msgError("transcribe_failed", err.Error()))
		return
	}
	if text == "" {
		c.send(msgSay("didn't catch that, bud."))
		return
	}
	c.send(msgTranscript(text, true))
	c.handleUtterance(text)
}

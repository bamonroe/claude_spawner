package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/detect"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// Audio contract for the voice path: the app sends `wake`, then binary PCM16LE
// frames (16 kHz, mono), then `audio_end`. The server assembles one WAV per
// utterance, transcribes it, echoes a `transcript`, and feeds the text through
// the same path as a typed `utterance`.
// The codec values a client may declare in `wake`. The same strings are
// mirrored as `Codecs` in the Kotlin client (net/Protocol.kt); a docsync test
// keeps the two sets (and docs/protocol.md) in agreement.
const (
	codecPCM16   = "pcm16"
	codecOggOpus = "ogg_opus"
)

const (
	audioSampleRate = 16000
	audioChannels   = 1
	// Cap a single utterance to ~120s of audio to bound memory (16k * 2 bytes/s).
	maxAudioBytes = audioSampleRate * 2 * 120
	// Cap the hands-free accumulation buffer to ~5 min. Hands-free appends every
	// gated clip's PCM until the end token is spoken; if the token is never
	// recognized (the runaway that end-token calibration guards against) this
	// would otherwise grow without bound at ~32 KB/s.
	maxHandsFreePCM = audioSampleRate * 2 * 300
)

// startAudio begins recording an utterance. codec is "ogg_opus" (the app's
// compressed default) or "pcm16" (raw); empty means pcm16. handsFree marks a
// VAD-gated clip so the server can drop background speech (no wake word, outside
// the follow-up window) instead of acting on it.
func (c *conn) startAudio(codec string, handsFree, calibrate bool) {
	switch codec {
	case "":
		codec = codecPCM16
	case codecPCM16, codecOggOpus:
	default:
		c.fail("bad_message", "unsupported audio codec "+strconv.Quote(codec))
		return
	}
	if c.transcriber() == nil {
		c.fail("not_implemented", "audio transcription is disabled; send text as 'utterance'")
		return
	}
	c.collecting = true
	c.gated = handsFree
	c.calibrate = calibrate
	c.training = false
	c.audio = c.audio[:0]
	c.audioCodec = codec
}

// startTrainClip begins recording a labeled wake/end-token training sample (the
// in-app "add live training data" flow). The clip is saved to disk, not
// transcribed or dispatched. model is the token model ("bump_bump"|"beep_beep"),
// category the bucket ("positive"|"negative"|"background"), label the phrase read.
func (c *conn) startTrainClip(codec, model, category, label string) {
	switch codec {
	case "":
		codec = codecPCM16
	case codecPCM16, codecOggOpus:
	default:
		c.fail("bad_message", "unsupported audio codec "+strconv.Quote(codec))
		return
	}
	if c.srv.cfg.WakewordTrainDir == "" {
		c.fail("not_implemented", "training-data capture is disabled (set SPAWNER_WAKEWORD_TRAIN_DIR)")
		return
	}
	if !validTrainModel(model) || !validTrainCategory(category) || strings.TrimSpace(label) == "" {
		c.fail("bad_message", "train_clip needs a valid model, category, and label")
		return
	}
	c.collecting = true
	c.gated = false
	c.calibrate = false
	c.training = true
	c.trainModel = model
	c.trainCat = category
	c.trainLabel = label
	c.audio = c.audio[:0]
	c.audioCodec = codec
}

func validTrainModel(m string) bool {
	return m == detect.WakeModel || m == detect.EndModel
}

func validTrainCategory(cat string) bool {
	switch cat {
	case "positive", "negative", "background":
		return true
	}
	return false
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
		c.fail("not_implemented", "audio transcription is disabled")
		return
	}
	if len(c.audio) == 0 {
		if !c.gated {
			c.send(msgSay("didn't hear anything."))
		}
		return
	}

	// Decode this clip to raw PCM16 (16 kHz mono).
	var pcm []byte
	if c.audioCodec == codecOggOpus {
		decoded, derr := transcribe.OggOpusToPCM(c.srv.cfg.FfmpegBin, c.audio)
		if derr != nil {
			c.fail("transcribe_failed", derr.Error())
			return
		}
		pcm = decoded
	} else {
		pcm = append([]byte(nil), c.audio...) // already PCM16LE 16 kHz mono
	}

	// Training capture: persist the labeled clip as a 16 kHz mono WAV (the
	// trainer's native rate) and ack — no transcription, no dispatch.
	if c.training {
		path, err := c.saveTrainClip(pcm)
		if err != nil {
			c.fail("bad_path", err.Error())
			return
		}
		c.send(msgTrainSaved(path, c.trainLabel))
		return
	}

	// Calibration: transcribe with the fast (detection) model and just report
	// what it heard — this measures exactly what end-token detection sees.
	if c.calibrate {
		text, _ := c.fastTranscriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
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
		transcribe.Options{Mode: c.sttMode, Model: c.sttModel, Prompt: c.vocabBias()})
	if err != nil {
		c.fail("transcribe_failed", err.Error())
		return
	}
	if text == "" {
		c.send(msgSay("didn't catch that."))
		return
	}
	c.send(msgTranscript(text, true))
	c.handleUtterance(text)
}

// saveTrainClip writes the recorded PCM as a labeled 16 kHz mono WAV under
// <WakewordTrainDir>/<model>/<category>/clip_<unixmillis>_<label-slug>.wav and
// returns the path. Model/category are validated in startTrainClip, so they're
// safe path segments; the label is slugged to stay filesystem-safe.
func (c *conn) saveTrainClip(pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", fmt.Errorf("no audio recorded")
	}
	dir := filepath.Join(c.srv.cfg.WakewordTrainDir, c.trainModel, c.trainCat)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("clip_%d_%s.wav", time.Now().UnixMilli(), slugify(c.trainLabel))
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// slugify reduces a spoken label ("beep beep") to a filesystem-safe token
// ("beep-beep"): lowercase, non-alphanumerics collapsed to single dashes.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "clip"
	}
	return out
}

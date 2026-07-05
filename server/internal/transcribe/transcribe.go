// Package transcribe turns an utterance's audio into text. The gateway depends
// only on the Transcriber interface, so the engine (whisper.cpp today) can be
// swapped for faster-whisper or a cloud API later without touching the gateway.
//
// Audio contract: the gateway hands us a complete WAV (PCM16LE, 16 kHz, mono) —
// one per utterance, assembled between the `wake` and `audio_end` messages.
package transcribe

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Options selects how a clip is transcribed. Mode "" or "dynamic" picks the
// model by clip length (fast for short, accurate for long); "fixed" always uses
// the named Model ("tiny" | "base" | "small").
type Options struct {
	Mode  string
	Model string
	// Prompt biases decoding toward these words (whisper's initial-prompt / vocab
	// hint) — e.g. session and project names, which STT otherwise mangles.
	Prompt string
}

// Transcriber converts a WAV clip into text.
type Transcriber interface {
	Transcribe(ctx context.Context, wav []byte, opt Options) (string, error)
}

// WhisperCPP shells out to the whisper.cpp CLI (one process per utterance),
// mirroring how the rest of the server shells out to `claude`.
type WhisperCPP struct {
	// Bin is the whisper.cpp binary (default "whisper-cli"; older builds: "main").
	Bin string
	// Model is the path to the primary (accurate) ggml model. Required.
	Model string
	// FastModel is an optional smaller/faster model for short clips. Empty =
	// always use Model.
	FastModel string
	// BaseModel is an optional middle model, selectable in "fixed" mode.
	BaseModel string
	// FastMaxSeconds is the clip-length cutoff for using FastModel.
	FastMaxSeconds float64
	// Lang biases decoding, e.g. "en". Empty lets whisper auto-detect.
	Lang string
	// Threads caps CPU threads (0 = whisper default).
	Threads int
}

// pickModel chooses the fast model for short clips, else the accurate one. The
// WAV is PCM16 mono 16 kHz, so duration ≈ bytes / (16000*2).
func (w *WhisperCPP) pickModel(wav []byte) string {
	if w.FastModel == "" || w.FastMaxSeconds <= 0 {
		return w.Model
	}
	seconds := float64(len(wav)) / (16000.0 * 2.0)
	if seconds <= w.FastMaxSeconds {
		return w.FastModel
	}
	return w.Model
}

// modelFor maps a fixed-model key to its path ("" if not configured).
func (w *WhisperCPP) modelFor(key string) string {
	switch key {
	case "tiny":
		return w.FastModel
	case "base":
		return w.BaseModel
	case "small":
		return w.Model
	}
	return ""
}

// chooseModel resolves the model path for a clip given the caller's options.
func (w *WhisperCPP) chooseModel(wav []byte, opt Options) string {
	if opt.Mode == "fixed" {
		if m := w.modelFor(opt.Model); m != "" {
			return m
		}
	}
	return w.pickModel(wav)
}

// Transcribe writes the WAV to a temp file and runs whisper.cpp, returning the
// recognized text (timestamps suppressed).
func (w *WhisperCPP) Transcribe(ctx context.Context, wav []byte, opt Options) (string, error) {
	if w.Model == "" {
		return "", fmt.Errorf("no whisper model configured")
	}
	path, cleanup, err := writeTempFile("utterance-*.wav", wav)
	if err != nil {
		return "", err
	}
	defer cleanup()

	bin := w.Bin
	if bin == "" {
		bin = "whisper-cli"
	}
	model := w.chooseModel(wav, opt)
	log.Printf("whisper: %.1fs clip -> %s (%s)", float64(len(wav))/32000.0, filepath.Base(model), modeLabel(opt))
	args := []string{"-m", model, "-f", path, "-nt", "-np"}
	if opt.Prompt != "" {
		args = append(args, "--prompt", opt.Prompt)
	}
	if w.Lang != "" {
		args = append(args, "-l", w.Lang)
	}
	if w.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(w.Threads))
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("whisper %s: %w: %s", bin, err, strings.TrimSpace(stderr.String()))
	}
	return clean(string(out)), nil
}

func modeLabel(opt Options) string {
	if opt.Mode == "fixed" {
		return "fixed:" + opt.Model
	}
	return "dynamic"
}

// clean collapses whisper.cpp's output to a single trimmed line and drops the
// non-speech markers it emits for silence.
func clean(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	for _, marker := range []string{"[BLANK_AUDIO]", "[ Silence ]", "(silence)"} {
		s = strings.ReplaceAll(s, marker, "")
	}
	return strings.TrimSpace(s)
}

// OggOpusToPCM decodes an Ogg/Opus clip (what the app records over cellular) to
// raw little-endian PCM16 (16 kHz mono) — no WAV header — so chunks can be
// concatenated and transcribed as one whole-message clip. whisper can't read
// Opus, and Opus is ~10x smaller than raw PCM for speech. Wrap the result with
// PCM16WAV to get a WAV for whisper.
func OggOpusToPCM(ffmpegBin string, ogg []byte) ([]byte, error) {
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	path, cleanup, err := writeTempFile("utterance-*.ogg", ogg)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Bound the decode so a hung ffmpeg (corrupt clip, wedged process) can't pin a
	// goroutine forever — a clip is seconds of audio, so 30s is a generous ceiling.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-i", path,
		"-ar", "16000", "-ac", "1", "-f", "s16le", "pipe:1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg opus decode: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// writeTempFile writes data to a fresh temp file (named by pattern) and returns
// its path plus a cleanup func that removes it. The file is closed before
// returning, so path is safe to hand to an exec'd process; cleanup also runs if
// the write itself fails.
func writeTempFile(pattern string, data []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", func() {}, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}

// PCM16WAV wraps raw little-endian PCM16 samples in a canonical 44-byte WAV
// header. This is what the gateway feeds the Transcriber.
func PCM16WAV(pcm []byte, sampleRate, channels int) []byte {
	const bitsPerSample = 16
	byteRate := sampleRate * channels * bitsPerSample / 8
	blockAlign := channels * bitsPerSample / 8

	var b bytes.Buffer
	b.Grow(44 + len(pcm))
	le := binary.LittleEndian

	b.WriteString("RIFF")
	writeU32(&b, le, uint32(36+len(pcm))) // chunk size
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	writeU32(&b, le, 16)                 // PCM fmt chunk size
	writeU16(&b, le, 1)                  // audio format = PCM
	writeU16(&b, le, uint16(channels))   //
	writeU32(&b, le, uint32(sampleRate)) //
	writeU32(&b, le, uint32(byteRate))   //
	writeU16(&b, le, uint16(blockAlign)) //
	writeU16(&b, le, bitsPerSample)      //
	b.WriteString("data")
	writeU32(&b, le, uint32(len(pcm)))
	b.Write(pcm)
	return b.Bytes()
}

func writeU32(b *bytes.Buffer, o binary.ByteOrder, v uint32) {
	var tmp [4]byte
	o.PutUint32(tmp[:], v)
	b.Write(tmp[:])
}

func writeU16(b *bytes.Buffer, o binary.ByteOrder, v uint16) {
	var tmp [2]byte
	o.PutUint16(tmp[:], v)
	b.Write(tmp[:])
}

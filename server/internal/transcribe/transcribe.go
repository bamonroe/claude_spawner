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
	// -nc (no-context) stops whisper from seeding each 30s window with the
	// previous window's text: that carry-forward is what sustains a repetition
	// hallucination across a long clip ("X. X. X. …"). See collapseRepeats for
	// the text-level safety net that catches loops within a single window.
	args := []string{"-m", model, "-f", path, "-nt", "-np", "-nc"}
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

// clean collapses whisper.cpp's output to a single trimmed line, drops the
// non-speech markers it emits for silence, and collapses repetition-loop
// hallucinations (see collapseRepeats).
func clean(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	for _, marker := range []string{"[BLANK_AUDIO]", "[ Silence ]", "(silence)"} {
		s = strings.ReplaceAll(s, marker, "")
	}
	return collapseRepeats(strings.TrimSpace(s))
}

// collapseRepeats undoes whisper's repetition-loop hallucination, where the
// decoder gets stuck emitting the same phrase over and over ("X. X. X. X. …").
// It works on two levels, both conservative enough to leave normal speech alone:
//
//   - Consecutive identical sentences (split on . ! ?) collapse to one. Genuine
//     speech almost never repeats a whole sentence back-to-back, and losing one
//     accidental duplicate is harmless.
//   - Within a run with no sentence punctuation, a short phrase repeated 3+ times
//     in a row collapses to a single copy — catches loops like "go go go go …".
//
// Non-adjacent repeats are preserved, so legitimately recurring words survive.
func collapseRepeats(s string) string {
	if s == "" {
		return s
	}
	sentences := splitSentences(s)
	out := make([]string, 0, len(sentences))
	var prevKey string
	for _, sent := range sentences {
		key := normalizeForRepeat(sent)
		if key != "" && key == prevKey {
			continue // drop a back-to-back duplicate sentence
		}
		out = append(out, collapsePhraseRuns(sent))
		if key != "" {
			prevKey = key
		}
	}
	return strings.TrimSpace(strings.Join(out, " "))
}

// splitSentences breaks text into sentences, keeping each sentence's trailing
// . ! ? punctuation attached. A final run with no terminator is its own segment.
func splitSentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c == '.' || c == '!' || c == '?' {
			// absorb any run of terminators/spaces so "X.  Y" splits cleanly
			j := i + 1
			for j < len(s) && (s[j] == '.' || s[j] == '!' || s[j] == '?' || s[j] == ' ') {
				j++
			}
			if seg := strings.TrimSpace(s[start:j]); seg != "" {
				out = append(out, seg)
			}
			start = j
			i = j - 1
		}
	}
	if seg := strings.TrimSpace(s[start:]); seg != "" {
		out = append(out, seg)
	}
	return out
}

// normalizeForRepeat lowercases a sentence and strips punctuation/spacing so two
// sentences that differ only in trailing punctuation compare equal.
func normalizeForRepeat(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// collapsePhraseRuns collapses an immediately-repeated phrase (1..6 words)
// occurring 3+ times in a row down to a single copy, for loops that lack the
// sentence punctuation the sentence-level pass keys on.
func collapsePhraseRuns(s string) string {
	words := strings.Fields(s)
	if len(words) < 3 {
		return s
	}
	out := make([]string, 0, len(words))
	i := 0
	for i < len(words) {
		collapsed := false
		// Prefer the longest phrase so "a b a b a b" collapses as "a b", not "a".
		maxLen := 6
		if maxLen > (len(words)-i)/3 {
			maxLen = (len(words) - i) / 3
		}
		for plen := maxLen; plen >= 1; plen-- {
			reps := 1
			for i+plen*(reps+1) <= len(words) && phraseEq(words, i, i+plen*reps, plen) {
				reps++
			}
			if reps >= 3 {
				out = append(out, words[i:i+plen]...)
				i += plen * reps
				collapsed = true
				break
			}
		}
		if !collapsed {
			out = append(out, words[i])
			i++
		}
	}
	return strings.Join(out, " ")
}

// phraseEq reports whether the plen-word phrases at word offsets a and b match
// (case-insensitive).
func phraseEq(words []string, a, b, plen int) bool {
	for k := 0; k < plen; k++ {
		if !strings.EqualFold(words[a+k], words[b+k]) {
			return false
		}
	}
	return true
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

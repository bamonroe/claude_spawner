package transcribe

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestPCM16WAVHeader(t *testing.T) {
	pcm := make([]byte, 320) // 10ms @ 16kHz mono
	wav := PCM16WAV(pcm, 16000, 1)

	if len(wav) != 44+len(pcm) {
		t.Fatalf("wav length = %d, want %d", len(wav), 44+len(pcm))
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " {
		t.Fatalf("bad RIFF/WAVE/fmt magic: %q", wav[0:16])
	}
	if string(wav[36:40]) != "data" {
		t.Fatalf("missing data chunk: %q", wav[36:40])
	}
	le := binary.LittleEndian
	if got := le.Uint32(wav[24:28]); got != 16000 {
		t.Errorf("sample rate = %d, want 16000", got)
	}
	if got := le.Uint16(wav[22:24]); got != 1 {
		t.Errorf("channels = %d, want 1", got)
	}
	if got := le.Uint16(wav[34:36]); got != 16 {
		t.Errorf("bits/sample = %d, want 16", got)
	}
	if got := le.Uint32(wav[40:44]); int(got) != len(pcm) {
		t.Errorf("data size = %d, want %d", got, len(pcm))
	}
}

func TestWhisperCPPShellOut(t *testing.T) {
	// Fake whisper-cli: prints a fixed transcription plus a non-speech marker
	// to confirm clean() strips it.
	dir := t.TempDir()
	bin := filepath.Join(dir, "fakewhisper.sh")
	script := "#!/bin/sh\necho ' hello world '\necho '[BLANK_AUDIO]'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	w := &WhisperCPP{Bin: bin, Model: filepath.Join(dir, "model.bin")}
	// Model must be non-empty (path need not exist for the fake).
	got, err := w.Transcribe(context.Background(), PCM16WAV(make([]byte, 320), 16000, 1), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello world" {
		t.Fatalf("transcription = %q, want %q", got, "hello world")
	}
}

func TestWhisperCPPRequiresModel(t *testing.T) {
	w := &WhisperCPP{Bin: "whisper-cli"}
	if _, err := w.Transcribe(context.Background(), nil, Options{}); err == nil {
		t.Error("expected error when no model configured")
	}
}

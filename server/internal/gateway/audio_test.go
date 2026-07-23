package gateway

import (
	"testing"

	"github.com/gorilla/websocket"
)

func TestAudioPathTranscribesAndDispatches(t *testing.T) {
	stt := &fakeSTT{text: "hey buddy spawn a new session"}
	ts, _ := newTestServer(t, stt)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Simulate an utterance: wake, a couple PCM frames, audio_end.
	send(t, ws, map[string]any{"type": "wake"})
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "audio_end"})

	// The server should echo the transcript, then start the spawn dialog from it.
	tr := readUntil(t, ws, "transcript")
	if tr["text"] != stt.text {
		t.Fatalf("transcript = %v, want %q", tr["text"], stt.text)
	}
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_path" {
		t.Fatalf("expected await_path from transcribed utterance, got %v", d["state"])
	}
	// And the transcriber must have received a real WAV (RIFF header).
	if len(stt.gotWAV) < 44 || string(stt.gotWAV[:4]) != "RIFF" {
		t.Fatalf("transcriber did not get a WAV; got %d bytes", len(stt.gotWAV))
	}
}

func TestAudioRejectedWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t, nil) // no transcriber
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "wake"})
	m := readUntil(t, ws, "error")
	if m["code"] != "not_implemented" {
		t.Fatalf("expected not_implemented, got %v", m)
	}
}

func TestAudioUnknownCodecRejected(t *testing.T) {
	stt := &fakeSTT{text: "ignored"}
	ts, _ := newTestServer(t, stt)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "wake", "codec": "mp3"})
	m := readUntil(t, ws, "error")
	if m["code"] != "bad_message" {
		t.Fatalf("expected bad_message for unknown codec, got %v", m)
	}

	// The rejected wake must not have started collecting: audio_end with no
	// accepted wake is a no-op, not a transcription of stray frames.
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "audio_end"})
	send(t, ws, map[string]any{"type": "wake", "codec": "mp3"}) // fence: errors after audio_end ran
	readUntil(t, ws, "error")
	if len(stt.gotWAV) != 0 {
		t.Fatalf("transcriber ran on %d bytes after a rejected wake", len(stt.gotWAV))
	}
}

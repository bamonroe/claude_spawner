package tts

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSpeak checks the request shape Kokoro-FastAPI expects and that the
// response body streams back with its content type.
func TestSpeak(t *testing.T) {
	var got speechRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/audio/speech" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "audio/ogg")
		w.Write([]byte("OggS-fake-audio"))
	}))
	defer srv.Close()

	c := New(srv.URL, "af_heart", "opus")
	body, mime, err := c.Speak(context.Background(), "hello there", "", "")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	defer body.Close()
	audio, _ := io.ReadAll(body)
	if string(audio) != "OggS-fake-audio" {
		t.Errorf("audio = %q", audio)
	}
	if mime != "audio/ogg" {
		t.Errorf("mime = %q", mime)
	}
	if got.Model != "kokoro" || got.Input != "hello there" || !got.Stream {
		t.Errorf("request = %+v", got)
	}
	if got.Voice != "af_heart" {
		t.Errorf("voice = %q, want client default", got.Voice)
	}
	if got.ResponseFormat != "opus" {
		t.Errorf("response_format = %q", got.ResponseFormat)
	}
}

// TestSpeakVoiceOverride: an explicit voice wins over the client default.
func TestSpeakVoiceOverride(t *testing.T) {
	var got speechRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	c := New(srv.URL, "af_heart", "opus")
	body, _, err := c.Speak(context.Background(), "hi", "bf_emma", "")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	body.Close()
	if got.Voice != "bf_emma" {
		t.Errorf("voice = %q, want bf_emma", got.Voice)
	}
}

// TestSpeakFormatOverride: an explicit response format wins over the client
// default (per-request formats let each client kind pull its playback codec).
func TestSpeakFormatOverride(t *testing.T) {
	var got speechRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	c := New(srv.URL, "af_heart", "opus")
	body, _, err := c.Speak(context.Background(), "hi", "", "pcm")
	if err != nil {
		t.Fatalf("Speak: %v", err)
	}
	body.Close()
	if got.ResponseFormat != "pcm" {
		t.Errorf("response_format = %q, want pcm", got.ResponseFormat)
	}
}

// TestSpeakError: non-200 surfaces the server's message, body closed.
func TestSpeakError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `voice "nope" not found`, http.StatusBadRequest)
	}))
	defer srv.Close()
	c := New(srv.URL, "af_heart", "opus")
	if _, _, err := c.Speak(context.Background(), "hi", "nope", ""); err == nil {
		t.Fatal("want error on 400")
	}
}

// TestVoices parses the voice list.
func TestVoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/audio/voices" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"voices": []string{"af_heart", "bf_emma"}})
	}))
	defer srv.Close()
	c := New(srv.URL, "af_heart", "opus")
	voices, err := c.Voices(context.Background())
	if err != nil {
		t.Fatalf("Voices: %v", err)
	}
	if len(voices) != 2 || voices[0] != "af_heart" || voices[1] != "bf_emma" {
		t.Errorf("voices = %v", voices)
	}
}

func TestVoicesObjectShape(t *testing.T) {
	// The live Kokoro-FastAPI GPU image returns voice objects, not strings.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"voices": []map[string]string{
			{"id": "af_heart", "name": "af_heart"},
			{"id": "bf_emma", "name": "bf_emma"},
		}})
	}))
	defer srv.Close()
	c := New(srv.URL, "af_heart", "opus")
	voices, err := c.Voices(context.Background())
	if err != nil {
		t.Fatalf("Voices: %v", err)
	}
	if len(voices) != 2 || voices[0] != "af_heart" || voices[1] != "bf_emma" {
		t.Errorf("voices = %v", voices)
	}
}

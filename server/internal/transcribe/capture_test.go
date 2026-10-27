package transcribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRemoteWhisperSendsCapture: a Capture on Options arrives at the whisper
// endpoint as capture_* multipart fields (what the voice-capture proxy embeds in
// the archived WAV), alongside the normal inference fields.
func TestRemoteWhisperSendsCapture(t *testing.T) {
	var got map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse multipart: %v", err)
		}
		got = r.MultipartForm.Value
		w.Write([]byte(`{"text":" hello there "}`))
	}))
	defer srv.Close()

	alt, acc := 12.5, 4.0
	zone := time.FixedZone("EDT", -4*3600)
	cp := &Capture{
		Kind: CapturePartial, UtteranceID: "u-1", Seq: 3, Artist: "Brian Monroe",
		RecordedAt: time.Date(2026, 10, 27, 9, 30, 0, 0, zone),
		Location: &Location{Lat: 40.1, Lon: -75.25, AltM: &alt, AccuracyM: &acc,
			FixAt: time.Date(2026, 10, 27, 9, 29, 58, 0, zone), Provider: "fused"},
		ClientID: "cid", Platform: "android", AppVersion: "1.2.3", Device: "Google Pixel 8a",
		SessionID: "sid", SessionName: "spawner", SessionCwd: "/data/claude_spawner", Host: "local",
		Codec: "ogg_opus", HandsFree: true, Denoised: false, STTRole: "fast", STTModel: "base.en",
	}
	w := &RemoteWhisper{URL: srv.URL}
	wav := PCM16WAV(make([]byte, 16000), 16000, 1) // 0.5 s
	text, err := w.Transcribe(context.Background(), wav, Options{Prompt: "p", Capture: cp})
	if err != nil || text != "hello there" {
		t.Fatalf("Transcribe = %q, %v", text, err)
	}
	want := map[string]string{
		"capture_source": "claude_spawner", "capture_kind": "partial", "capture_utterance": "u-1",
		"capture_seq": "3", "capture_artist": "Brian Monroe",
		"capture_recorded_at": "2026-10-27T09:30:00-04:00",
		"capture_lat":         "40.1", "capture_lon": "-75.25", "capture_alt_m": "12.5", "capture_accuracy_m": "4",
		"capture_location_at": "2026-10-27T09:29:58-04:00", "capture_location_provider": "fused",
		"capture_client_id": "cid", "capture_platform": "android", "capture_app_version": "1.2.3",
		"capture_device": "Google Pixel 8a", "capture_session_id": "sid", "capture_session_name": "spawner",
		"capture_session_cwd": "/data/claude_spawner", "capture_host": "local", "capture_codec": "ogg_opus",
		"capture_hands_free": "true", "capture_denoised": "false", "capture_stt_role": "fast",
		"capture_stt_model": "base.en", "capture_duration_s": "0.500",
		"prompt": "p", "response_format": "json",
	}
	for k, v := range want {
		if g := got[k]; len(g) != 1 || g[0] != v {
			t.Errorf("field %s = %v, want %q", k, g, v)
		}
	}
}

// Empty fields are omitted, and no Capture means no capture_* fields at all.
func TestCaptureOmitsEmpty(t *testing.T) {
	var keys []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		keys = keys[:0]
		for k := range r.MultipartForm.Value {
			if strings.HasPrefix(k, "capture_") {
				keys = append(keys, k)
			}
		}
		w.Write([]byte(`{"text":"x"}`))
	}))
	defer srv.Close()
	w := &RemoteWhisper{URL: srv.URL}
	wav := PCM16WAV(make([]byte, 320), 16000, 1)

	if _, err := w.Transcribe(context.Background(), wav, Options{}); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("nil Capture sent fields %v", keys)
	}

	if _, err := w.Transcribe(context.Background(), wav, Options{Capture: &Capture{Kind: CaptureFinal}}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"capture_artist", "capture_lat", "capture_location_at", "capture_session_id", "capture_platform", "capture_seq"} {
		for _, g := range keys {
			if g == k {
				t.Errorf("empty field %s was sent", k)
			}
		}
	}
	if len(keys) != 5 { // source, kind, hands_free, denoised, duration_s
		t.Errorf("sparse capture sent %d fields %v, want 5", len(keys), keys)
	}
}

func TestWAVDuration(t *testing.T) {
	if d := WAVDuration(PCM16WAV(make([]byte, 64000), 16000, 1)); d != 2 {
		t.Errorf("2s WAV = %v", d)
	}
	if d := WAVDuration(make([]byte, 32000)); d != 1 { // headerless: the PCM16 16 kHz contract
		t.Errorf("headerless = %v", d)
	}
}

package detect

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRemoteWakewordDetect(t *testing.T) {
	var gotBody []byte
	var gotCT, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":{"bump_bump":0.02,"beep_beep":0.91}}`))
	}))
	defer srv.Close()

	d := &RemoteWakeword{URL: srv.URL + "/"} // trailing slash must be trimmed
	pcm := []byte{0x01, 0x02, 0x03, 0x04}
	scores, err := d.Detect(context.Background(), pcm)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if gotPath != "/detect" {
		t.Errorf("path = %q, want /detect", gotPath)
	}
	if gotCT != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", gotCT)
	}
	if string(gotBody) != string(pcm) {
		t.Errorf("body = %v, want raw pcm %v (must not be WAV-wrapped)", gotBody, pcm)
	}
	if scores[EndModel] != 0.91 || scores[WakeModel] != 0.02 {
		t.Errorf("scores = %v, want beep_beep 0.91 / bump_bump 0.02", scores)
	}
}

func TestRemoteWakewordDetectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model not loaded", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &RemoteWakeword{URL: srv.URL}
	if _, err := d.Detect(context.Background(), []byte{0x00, 0x00}); err == nil {
		t.Fatal("expected error on non-200, got nil")
	}
}

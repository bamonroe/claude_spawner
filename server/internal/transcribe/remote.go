package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// defaultRemoteClient bounds a hung whisper server from stalling a turn forever.
var defaultRemoteClient = &http.Client{Timeout: 120 * time.Second}

// LoadRemoteModel hot-swaps a resident whisper.cpp server to a different ggml
// model via its /load endpoint (no container restart). modelPath is the path as
// the whisper server sees it (e.g. /models/ggml-medium.en.bin).
func LoadRemoteModel(ctx context.Context, url, modelPath string) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("model", modelPath)
	mw.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+"/load", &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := defaultRemoteClient.Do(req)
	if err != nil {
		return fmt.Errorf("whisper load: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("whisper load response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("whisper load %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// RemoteWhisper transcribes by POSTing the WAV to a resident whisper.cpp server
// (its /inference endpoint), instead of forking whisper-cli per utterance. The
// server keeps the model loaded, so there's no per-call reload — same model,
// same accuracy, far less latency. One model per server, so Options.Mode/Model
// are ignored; Options.Capture rides along as `capture_*` form fields, which the
// voice-capture proxy in front of whisper embeds in each archived clip.
type RemoteWhisper struct {
	// URL is the whisper server base, e.g. "http://localhost:8572" (WhisperX).
	URL string
	// Client is the HTTP client (nil → a default with a generous timeout).
	Client *http.Client
}

func (w *RemoteWhisper) Transcribe(ctx context.Context, wav []byte, opt Options) (string, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "audio.wav")
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(wav); err != nil {
		return "", err
	}
	_ = mw.WriteField("response_format", "json")
	_ = mw.WriteField("temperature", "0.0")
	// Don't condition each window on the previous one — that carry-forward
	// (default max-context -1) sustains a repetition hallucination across a
	// long clip. The effective lever is max_context=0; no_context predates
	// whisper-server's schema and was never honored, sent for older servers.
	// collapseRepeats() remains the text-level backstop within one window.
	_ = mw.WriteField("max_context", "0")
	_ = mw.WriteField("no_context", "true")
	if opt.Prompt != "" {
		_ = mw.WriteField("prompt", opt.Prompt) // whisper.cpp server: initial-prompt bias
	}
	// Provenance for the archiving proxy; whisper-server ignores unknown fields.
	for _, kv := range opt.Capture.FormFields(wav) {
		_ = mw.WriteField(kv[0], kv[1])
	}
	mw.Close()

	url := strings.TrimRight(w.URL, "/") + "/inference"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := w.Client
	if client == nil {
		client = defaultRemoteClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("whisper server: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("whisper server response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("whisper server %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("whisper server bad json: %w (%s)", err, strings.TrimSpace(string(data)))
	}
	text := clean(out.Text)
	log.Printf("whisper(remote): %.1fs clip -> %q", WAVDuration(wav), text)
	return text, nil
}

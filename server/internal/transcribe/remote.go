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
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("whisper load %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// RemoteWhisper transcribes by POSTing the WAV to a resident whisper.cpp server
// (its /inference endpoint), instead of forking whisper-cli per utterance. The
// server keeps the model loaded, so there's no per-call reload — same model,
// same accuracy, far less latency. One model per server, so Options is ignored.
type RemoteWhisper struct {
	// URL is the whisper server base, e.g. "http://localhost:8571".
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
	if opt.Prompt != "" {
		_ = mw.WriteField("prompt", opt.Prompt) // whisper.cpp server: initial-prompt bias
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
	data, _ := io.ReadAll(resp.Body)
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
	log.Printf("whisper(remote): %.1fs clip -> %q", float64(len(wav))/32000.0, text)
	return text, nil
}

// Package tts is the thin client for the resident Kokoro TTS server
// (Kokoro-FastAPI, https://github.com/remsky/Kokoro-FastAPI — an
// OpenAI-compatible /v1/audio/speech). Same shape as transcribe.RemoteWhisper:
// a stateless HTTP wrapper; the compose stack runs the model server.
package tts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to one Kokoro-FastAPI server.
type Client struct {
	// URL is the server's base URL, e.g. http://localhost:8880.
	URL string
	// Voice is the default voice for requests that don't name one
	// (Kokoro voice ids like "af_heart"; weighted mixes such as
	// "af_bella(2)+af_sky(1)" pass through verbatim).
	Voice string
	// Format is the response_format asked of the server
	// (mp3 | wav | opus | flac | pcm).
	Format string
	// HTTP is the client used for requests. Speak streams long responses, so
	// this client must NOT set an overall Timeout — pass deadlines via ctx.
	HTTP *http.Client
}

// New returns a Client with a keep-alive HTTP client suitable for streaming.
func New(url, voice, format string) *Client {
	return &Client{URL: url, Voice: voice, Format: format, HTTP: &http.Client{}}
}

// speechRequest is the OpenAI-compatible synthesis request body.
type speechRequest struct {
	Model          string `json:"model"`
	Input          string `json:"input"`
	Voice          string `json:"voice"`
	ResponseFormat string `json:"response_format"`
	// Stream asks the server to emit audio as each sentence is synthesized,
	// so playback can start well before long replies finish.
	Stream bool `json:"stream"`
}

// Speak synthesizes text with the given voice and format (empty = the client
// defaults) and returns the audio stream plus its Content-Type. The caller
// must Close it. Per-request formats let each client kind pull the encoding
// its playback path wants (Android streams raw pcm into an AudioTrack; the
// browser decodes compressed mp3).
func (c *Client) Speak(ctx context.Context, text, voice, format string) (io.ReadCloser, string, error) {
	if voice == "" {
		voice = c.Voice
	}
	if format == "" {
		format = c.Format
	}
	body, err := json.Marshal(speechRequest{
		Model: "kokoro", Input: text, Voice: voice,
		ResponseFormat: format, Stream: true,
	})
	if err != nil {
		return nil, "", fmt.Errorf("tts: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/v1/audio/speech", bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("tts: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("tts: speech request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		return nil, "", fmt.Errorf("tts: speech request: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return resp.Body, resp.Header.Get("Content-Type"), nil
}

// Voices returns the server's voice ids (GET /v1/audio/voices), sorted as the
// server reports them. Callers surface this as the client-side voice picker.
func (c *Client) Voices(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/v1/audio/voices", nil)
	if err != nil {
		return nil, fmt.Errorf("tts: build request: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: voices request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("tts: voices request: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	// Kokoro-FastAPI has shipped both shapes: {"voices":["af_heart",...]} and
	// {"voices":[{"id":"af_heart","name":"af_heart"},...]}. Accept either.
	var out struct {
		Voices []json.RawMessage `json:"voices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("tts: decode voices: %w", err)
	}
	voices := make([]string, 0, len(out.Voices))
	for _, raw := range out.Voices {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			voices = append(voices, s)
			continue
		}
		var obj struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, fmt.Errorf("tts: decode voices: %w", err)
		}
		if obj.ID != "" {
			voices = append(voices, obj.ID)
		} else if obj.Name != "" {
			voices = append(voices, obj.Name)
		}
	}
	return voices, nil
}

package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// defaultClient bounds a hung sidecar from stalling a turn. Detection is a small,
// resident-model call (~one window), so the timeout is far tighter than Whisper's.
var defaultClient = &http.Client{Timeout: 15 * time.Second}

// RemoteWakeword scores a clip by POSTing raw PCM to the wakeword sidecar's
// /detect endpoint (SPAWNER_WAKEWORD_URL). The sidecar holds the model resident
// and left-pads short clips to its 2s window, so a bounded VAD/push-to-talk clip
// scores directly — no client-side framing. The body is raw little-endian i16
// mono at 16 kHz (NOT a WAV wrapper).
type RemoteWakeword struct {
	// URL is the sidecar base, e.g. "http://localhost:9060".
	URL string
	// Client is the HTTP client (nil → a default with a short timeout).
	Client *http.Client
}

func (d *RemoteWakeword) Detect(ctx context.Context, pcm []byte) (Scores, error) {
	url := strings.TrimRight(d.URL, "/") + "/detect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(pcm))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")

	client := d.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("wakeword sidecar: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("wakeword sidecar response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("wakeword sidecar %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Scores Scores `json:"scores"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("wakeword sidecar bad json: %w (%s)", err, strings.TrimSpace(string(data)))
	}
	return out.Scores, nil
}

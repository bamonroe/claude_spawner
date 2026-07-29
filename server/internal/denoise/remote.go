// Package denoise wraps the resident DeepFilterNet sidecar (SPAWNER_DENOISE_URL)
// that scrubs steady background noise — wind, road, engine, fan — out of a voice
// clip before it reaches Whisper. It's an optional, per-client-toggleable stage
// on the accurate-transcribe seam: the clip's raw PCM is wrapped as a WAV, POSTed
// to the sidecar's /denoise endpoint, and the enhanced WAV it returns is decoded
// back to the same 16 kHz mono PCM16 the rest of the pipeline assumes.
package denoise

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// defaultClient bounds a hung sidecar. DeepFilterNet runs resident on the GPU and
// enhances a few seconds of audio in well under a second, but the clip round-trips
// as WAV and is re-decoded, so the ceiling is generous without being unbounded.
var defaultClient = &http.Client{Timeout: 30 * time.Second}

const (
	audioSampleRate = 16000
	audioChannels   = 1
)

// Remote denoises a clip via the DeepFilterNet /denoise sidecar. The sidecar takes
// a multipart file upload (any ffmpeg-decodable audio), force-resamples to mono
// 48 kHz for the model, and returns a denoised 48 kHz mono WAV.
type Remote struct {
	// URL is the sidecar base, e.g. "http://localhost:8573".
	URL string
	// FfmpegBin decodes the returned 48 kHz WAV back down to 16 kHz PCM16.
	FfmpegBin string
	// Client is the HTTP client (nil → a default with a bounded timeout).
	Client *http.Client
}

// Denoise returns pcm (raw little-endian PCM16, 16 kHz mono) scrubbed of steady
// noise. attenDb caps the maximum noise attenuation in dB (DeepFilterNet's
// atten_lim_db — lower is gentler and preserves more of the original; <= 0 leaves
// it unset, i.e. full enhancement). The output is the same PCM16 16 kHz mono
// format as the input, so callers substitute it in place.
func (d *Remote) Denoise(ctx context.Context, pcm []byte, attenDb float64) ([]byte, error) {
	// Wrap the raw PCM as a WAV so the sidecar's ffmpeg step can read it.
	wav := transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", "clip.wav")
	if err != nil {
		return nil, err
	}
	if _, err := part.Write(wav); err != nil {
		return nil, err
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}

	url := strings.TrimRight(d.URL, "/") + "/denoise"
	if attenDb > 0 {
		url += "?atten_lim_db=" + strconv.FormatFloat(attenDb, 'f', -1, 64)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	client := d.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("denoise sidecar: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("denoise sidecar response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("denoise sidecar %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}

	// The sidecar returns a 48 kHz mono WAV; decode it back to 16 kHz PCM16.
	out, err := transcribe.WAVToPCM16(d.FfmpegBin, data)
	if err != nil {
		return nil, fmt.Errorf("denoise decode: %w", err)
	}
	return out, nil
}

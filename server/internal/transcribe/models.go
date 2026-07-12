package transcribe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// ModelInfo describes one downloadable whisper.cpp ggml model.
type ModelInfo struct {
	// Name is the ggml model name, e.g. "medium.en" → ggml-medium.en.bin.
	Name string
	// SizeMB is the approximate download size, for a picker's display only.
	SizeMB int
}

// EnglishModels is the curated, finite catalog of whisper.cpp models useful for
// English transcription, ordered smallest→largest. The `.en` variants are
// English-only; `large-v3[-turbo]` are multilingual but the strongest English
// models (there is no `large.en`). A name maps to ggml-<Name>.bin on Hugging
// Face (ggerganov/whisper.cpp) and to /models/ggml-<Name>.bin on the resident
// server, so the picker can offer every English model and fetch on demand.
var EnglishModels = []ModelInfo{
	{"tiny.en", 75},
	{"base.en", 142},
	{"small.en", 466},
	{"medium.en", 1533},
	{"large-v3-turbo", 1620},
	{"large-v3", 3094},
}

// modelBaseURL is the Hugging Face repo a ggml model file is fetched from. A var
// (not const) so tests can point it at a local server.
var modelBaseURL = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"

// IsCatalogModel reports whether name is in the English catalog — i.e. whether
// we know how to download it on demand.
func IsCatalogModel(name string) bool {
	for _, m := range EnglishModels {
		if m.Name == name {
			return true
		}
	}
	return false
}

// ModelFileName is the ggml filename for a model name (ggml-<name>.bin).
func ModelFileName(name string) string { return "ggml-" + name + ".bin" }

// downloadClient has no overall timeout: a multi-GB model over a slow link can
// take many minutes, so the request context bounds it instead of the client.
var downloadClient = &http.Client{}

// DownloadModel fetches ggml-<name>.bin from Hugging Face into dir, streaming to
// a temp file and renaming on success so a partial download never looks like a
// complete model. progress (may be nil) is called as bytes arrive with
// (received, total); total is 0 when the server sends no Content-Length. name
// must be in the catalog. A no-op (nil) if the file is already present.
func DownloadModel(ctx context.Context, dir, name string, progress func(received, total int64)) error {
	if !IsCatalogModel(name) {
		return fmt.Errorf("unknown model %q", name)
	}
	if dir == "" {
		return fmt.Errorf("no models directory configured")
	}
	final := filepath.Join(dir, ModelFileName(name))
	if _, err := os.Stat(final); err == nil {
		return nil // already present
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelBaseURL+ModelFileName(name), nil)
	if err != nil {
		return err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: http %d", name, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ModelFileName(name)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename below succeeds
	pw := &progressWriter{total: resp.ContentLength, cb: progress}
	if _, err := io.Copy(tmp, io.TeeReader(resp.Body, pw)); err != nil {
		tmp.Close()
		return fmt.Errorf("download %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}

// progressWriter counts bytes copied and forwards a throttled progress callback
// (at most every ~500ms) so a broadcast per network packet doesn't flood clients.
type progressWriter struct {
	total    int64
	received int64
	cb       func(received, total int64)
	last     time.Time
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.received += int64(len(p))
	if w.cb != nil {
		now := time.Now()
		if now.Sub(w.last) >= 500*time.Millisecond {
			w.last = now
			w.cb(w.received, w.total)
		}
	}
	return len(p), nil
}

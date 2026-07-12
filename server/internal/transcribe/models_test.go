package transcribe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestModelCatalog(t *testing.T) {
	if !IsCatalogModel("medium.en") {
		t.Error("medium.en should be a catalog model")
	}
	if IsCatalogModel("not-a-real-model") {
		t.Error("bogus name should not be a catalog model")
	}
	if got := ModelFileName("medium.en"); got != "ggml-medium.en.bin" {
		t.Errorf("ModelFileName = %q, want ggml-medium.en.bin", got)
	}
	// Catalog is ordered small→large so a picker reads sensibly.
	for i := 1; i < len(EnglishModels); i++ {
		if EnglishModels[i].SizeMB < EnglishModels[i-1].SizeMB {
			t.Errorf("catalog not size-ordered at %d: %+v", i, EnglishModels)
		}
	}
}

func TestDownloadModel(t *testing.T) {
	const payload = "ggml-model-bytes"
	var gotProgress bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ggml-tiny.en.bin" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(payload))
	}))
	defer srv.Close()

	orig := modelBaseURL
	modelBaseURL = srv.URL + "/"
	defer func() { modelBaseURL = orig }()

	dir := t.TempDir()
	err := DownloadModel(context.Background(), dir, "tiny.en", func(received, total int64) { gotProgress = true })
	if err != nil {
		t.Fatalf("DownloadModel: %v", err)
	}
	_ = gotProgress // throttling may skip the single tiny chunk; not asserted
	data, err := os.ReadFile(filepath.Join(dir, "ggml-tiny.en.bin"))
	if err != nil {
		t.Fatalf("read downloaded model: %v", err)
	}
	if string(data) != payload {
		t.Errorf("downloaded %q, want %q", data, payload)
	}
	// Second call is a no-op (already present), not a re-download.
	if err := DownloadModel(context.Background(), dir, "tiny.en", nil); err != nil {
		t.Fatalf("DownloadModel (present): %v", err)
	}
	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}

	if err := DownloadModel(context.Background(), dir, "bogus", nil); err == nil {
		t.Error("expected error for non-catalog model")
	}
}

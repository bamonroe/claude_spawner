package gateway

import (
	"os"
	"path/filepath"
	"testing"
)

// Browse is served off the inbound loop (startBrowse), so several taps can be in
// flight at once and each still gets its own reply — nothing is lost or reordered
// away by the concurrency.
func TestBrowseConcurrentRequestsAllAnswered(t *testing.T) {
	ts, root := newTestServer(t, nil)
	dirs := []string{"alpha", "beta", "gamma"}
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(root, d, "kid"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	for _, d := range dirs {
		send(t, ws, map[string]any{"type": "browse", "path": filepath.Join(root, d)})
	}
	want := map[string]bool{}
	for _, d := range dirs {
		want[filepath.Join(root, d)] = true
	}
	for range dirs {
		m := readUntil(t, ws, "listing")
		path, _ := m["path"].(string)
		if !want[path] {
			t.Fatalf("unexpected or duplicate listing for %q", path)
		}
		delete(want, path)
	}
	if len(want) != 0 {
		t.Fatalf("missing listings: %v", want)
	}
}

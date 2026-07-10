package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gorilla/websocket"
)

// spawnSession runs the spawn dialog to create + attach a session under root,
// returning its name. Mirrors the flow in TestSpawnDialogAndDictation.
func spawnSession(t *testing.T, ws *websocket.Conn, root string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy, spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "proj"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	a := readUntil(t, ws, "attached")
	name, _ := a["name"].(string)
	if name == "" {
		t.Fatal("expected a session name")
	}
	return name
}

// TestDigestAndHistoryUnchanged drives the offline-cache wire end to end through
// the real gateway/driver/store: a `digest` sweep returns the session with a
// content hash, and a `history` top page carrying that hash comes back
// `unchanged` with no bodies (while a hashless request returns the full page and
// the same digest for the app to store). The session's transcript is empty here,
// so this exercises the plumbing and the empty-chain digest specifically.
func TestDigestAndHistoryUnchanged(t *testing.T) {
	ts, root := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	name := spawnSession(t, ws, root)

	// digest -> digests: the session appears with a hash.
	send(t, ws, map[string]any{"type": "digest"})
	dg := readUntil(t, ws, "digests")
	items, _ := dg["items"].([]any)
	var hash string
	var found bool
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["name"] == name {
			found = true
			hash, _ = m["hash"].(string)
		}
	}
	if !found {
		t.Fatalf("session %q not in digests: %v", name, dg["items"])
	}
	if hash == "" {
		t.Fatal("digest hash is empty")
	}

	// history with the matching have_hash -> unchanged, no bodies.
	send(t, ws, map[string]any{"type": "history", "name": name, "have_hash": hash})
	h := readUntil(t, ws, "history")
	if h["unchanged"] != true {
		t.Fatalf("expected unchanged=true, got %v", h["unchanged"])
	}
	if msgs, _ := h["messages"].([]any); len(msgs) != 0 {
		t.Fatalf("expected no bodies on an unchanged reply, got %v", msgs)
	}
	if h["hash"] != hash {
		t.Fatalf("unchanged reply hash %v != digest hash %v", h["hash"], hash)
	}

	// history with the WRONG have_hash -> a real page (unchanged=false) carrying
	// the current digest for the app to store.
	send(t, ws, map[string]any{"type": "history", "name": name, "have_hash": "stale"})
	h = readUntil(t, ws, "history")
	if h["unchanged"] == true {
		t.Fatal("a stale have_hash must not report unchanged")
	}
	if h["hash"] != hash {
		t.Fatalf("page reply hash %v != digest hash %v", h["hash"], hash)
	}
}

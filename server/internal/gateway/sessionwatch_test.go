package gateway

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// A session registered by one client must reach every OTHER connected client as
// an unsolicited `discovered` push — that's the whole point of the watcher: the
// second device's sidebar and radial menu update without a refresh tap.
func TestSessionChangeBroadcastsDiscovered(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	a := dial(t, ts)
	send(t, a, map[string]any{"type": "hello", "token": "secret", "client_id": "a"})
	readUntil(t, a, "hello_ok")
	b := dial(t, ts)
	send(t, b, map[string]any{"type": "hello", "token": "secret", "client_id": "b"})
	readUntil(t, b, "hello_ok")

	dir := filepath.Join(root, "pushed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "11111111-2222-3333-4444-555555555555"
	if _, err := gw.registerDiscovered(id, dir); err != nil {
		t.Fatal(err)
	}
	// Neither client asked for this; both must be told anyway.
	waitPushedSession(t, b, id)
	waitPushedSession(t, a, id)
}

// The push must not depend on a disk walk: with a cold memo (this server has
// never scanned) the registered rows still go out, flagged partial so the client
// keeps whatever adoptable rows it already has instead of pruning them.
func TestSessionPushIsPartialWithoutAWalk(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "client_id": "solo"})
	readUntil(t, ws, "hello_ok")

	dir := filepath.Join(root, "cold")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if _, err := gw.registerDiscovered(id, dir); err != nil {
		t.Fatal(err)
	}
	m := waitPushedSession(t, ws, id)
	if partial, _ := m["partial"].(bool); !partial {
		t.Fatalf("cold-memo push should be partial: %v", m)
	}
}

// waitPushedSession reads until a `discovered` frame carrying sessionID arrives,
// skipping any frame the connection was already owed (the hello handshake sends
// its own lists).
func waitPushedSession(t *testing.T, ws *websocket.Conn, sessionID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	_ = ws.SetReadDeadline(deadline)
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for a discovered push carrying %s: %v", sessionID, err)
		}
		if m["type"] != "discovered" {
			continue
		}
		items, _ := m["sessions"].([]any)
		for _, it := range items {
			row, _ := it.(map[string]any)
			if row["session_id"] == sessionID {
				return m
			}
		}
	}
}

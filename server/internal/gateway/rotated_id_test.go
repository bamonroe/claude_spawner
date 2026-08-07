package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// TestRotatedIDResolvesToLiveSession reproduces the phantom-duplicate-session
// bug: a clear/auto-compress rotates the live session_id and retires the old
// one, but the app keeps routing by the pre-rotation id. Every id-holding path
// must resolve that stale id back to the SAME session — attach must not fall
// through to adoption (which minted a second session named after the
// directory), registerDiscovered must not mint a duplicate for a retired id
// whose transcript is still on disk, and the utterance path must re-key the
// client onto the current id.
func TestRotatedIDResolvesToLiveSession(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": dir})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	oldID, _ := readUntil(t, ws, "attached")["session_id"].(string)
	if oldID == "" {
		t.Fatal("expected a session_id on attach")
	}

	// Rotate the id the way a "clear" does (clear refuses an unstarted session,
	// so mark it started first).
	rec := gw.store.GetBySessionID(oldID)
	rec.Mutate(func(s *session.Session) { s.Started = true })
	send(t, ws, map[string]any{"type": "clear"})
	newID, _ := readUntil(t, ws, "context_reset")["session_id"].(string)
	if newID == "" || newID == oldID {
		t.Fatalf("clear should rotate the session_id (got %q, was %q)", newID, oldID)
	}

	// Attach by the RETIRED id: must resolve to the same session and answer with
	// the CURRENT id so the client re-keys — never adopt a phantom duplicate.
	send(t, ws, map[string]any{"type": "attach", "session_id": oldID})
	if got, _ := readUntil(t, ws, "attached")["session_id"].(string); got != newID {
		t.Fatalf("attach by retired id: attached carries %q, want current id %q", got, newID)
	}

	// registerDiscovered on the retired id (its transcript stays on disk, so
	// discovery keeps offering it) must return the owning record, not mint a
	// second session named after the directory.
	before := len(gw.store.List())
	adopted, err := gw.registerDiscovered(oldID, dir)
	if err != nil {
		t.Fatal(err)
	}
	if adopted != rec {
		t.Fatalf("registerDiscovered minted a new record for a retired id (name %q)", recName(adopted))
	}
	if after := len(gw.store.List()); after != before {
		t.Fatalf("registerDiscovered grew the store %d -> %d on a retired id", before, after)
	}

	// An utterance declaring the stale id must stay on the session and re-send
	// `attached` with the current id so the client stops routing by the old one.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy status", "session_id": oldID})
	if got, _ := readUntil(t, ws, "attached")["session_id"].(string); got != newID {
		t.Fatalf("stale-id utterance: attached carries %q, want current id %q", got, newID)
	}
}

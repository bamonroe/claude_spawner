package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// fakeCodex is a stand-in for `codex exec --json`: it mints its OWN thread id
// (announced on thread.started, as the real CLI does) and answers with a fixed
// reply. Driving a turn through it exercises the SelfAssignsID path, where the
// session's id rotates mid-turn.
func fakeCodex(t *testing.T, threadID, reply string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakecodex.sh")
	script := "#!/bin/sh\n" +
		`echo '{"type":"thread.started","thread_id":"` + threadID + `"}'` + "\n" +
		`echo '{"type":"item.completed","item":{"type":"agent_message","text":"` + reply + `"}}'` + "\n" +
		`echo '{"type":"turn.completed"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSelfAssignedIDRotationKeepsLiveOutput reproduces the "first reply after the
// backend switch doesn't arrive live" bug. A self-assigning backend (codex) adopts
// its own session id during the turn, but the job hub is keyed by the id the turn
// STARTED with — so without a re-key the NEXT turn builds a fresh hub with no
// sinks and its reply reaches no attached device (it only shows up when the
// session is reopened and history refetched). The client also has to be told the
// rotated id, since every hub frame is stamped with it.
func TestSelfAssignedIDRotationKeepsLiveOutput(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	gw.driver.AgentBins = map[string]map[session.Target]string{
		"codex": {session.TargetHost: fakeCodex(t, "thread-abc", "pong")},
	}

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "proj")})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	spawnID, _ := readUntil(t, ws, "attached")["session_id"].(string)
	if spawnID == "" {
		t.Fatal("expected a session_id on attach")
	}

	// Switch the session onto the self-assigning backend.
	send(t, ws, map[string]any{"type": "set_agent", "session_id": spawnID, "agent": "codex"})
	switchedID, _ := readUntil(t, ws, "attached")["session_id"].(string)
	if switchedID == "" || switchedID == spawnID {
		t.Fatalf("set_agent should rotate the session_id (got %q, was %q)", switchedID, spawnID)
	}

	// First codex turn: the backend mints "thread-abc" and the record adopts it. The
	// client must be told, or it keeps filing this session's output under the
	// retired id.
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong", "session_id": switchedID})
	if out := readUntil(t, ws, "output"); out["text"] != "pong" {
		t.Fatalf("first codex turn: want pong, got %v", out["text"])
	}
	// The bug: the second turn's reply never reached the attached device, because
	// the hub was still filed under the pre-adoption id.
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong", "session_id": "thread-abc"})
	out := readUntil(t, ws, "output")
	if out["text"] != "pong" {
		t.Fatalf("second codex turn: want pong, got %v", out["text"])
	}
	if sid, _ := out["session_id"].(string); sid != "thread-abc" {
		t.Fatalf("turn frames stamped %q, want the adopted id %q", sid, "thread-abc")
	}
}

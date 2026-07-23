package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

func TestRenameSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")
	// Spawn + attach to create a session record.
	old := spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))

	// Rename it and expect the refreshed session_list to carry the new name.
	send(t, ws, map[string]any{"type": "rename", "name": old, "new_name": "renamed"})
	m := readUntil(t, ws, "session_list")
	sl, _ := m["sessions"].([]any)
	found := false
	for _, s := range sl {
		if s.(map[string]any)["name"] == "renamed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("renamed session not in list: %v", m["sessions"])
	}

	// The job hub is keyed by session_id (stable across the rename), so a turn
	// dictated after the rename must still fan out to this attached connection —
	// no hub re-keying required. Regression guard for dropping renameJob.
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong"})
	if out := readUntil(t, ws, "output"); out["text"] != "pong" {
		t.Fatalf("no turn output after rename (hub lost across rename?): %v", out["text"])
	}
}

// TestRenameBroadcastsToAllAttachedConns proves a rename fans the `renamed` title
// update out to EVERY connection attached to that session — the initiator plus any
// other device the user has on the same session (they run a phone AND a tablet at
// once) — so each client updates its title in place rather than inferring the
// rename from a later discovered-list diff. A connection attached to a DIFFERENT
// session must not receive it.
func TestRenameBroadcastsToAllAttachedConns(t *testing.T) {
	ts, root := newTestServer(t, nil)
	for _, d := range []string{"projone", "projtwo"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Conn A: the initiator — spawn + attach session S.
	a := dial(t, ts)
	send(t, a, map[string]any{"type": "hello", "token": "secret", "client_id": "devA"})
	readUntil(t, a, "hello_ok")
	sName := spawnAttachVoice(t, a, filepath.Join(root, "projone"))

	// Conn C: a second user device attached to a DIFFERENT session T.
	c := dial(t, ts)
	send(t, c, map[string]any{"type": "hello", "token": "secret", "client_id": "devC"})
	readUntil(t, c, "hello_ok")
	spawnAttachVoice(t, c, filepath.Join(root, "projtwo"))

	// Conn B: a second device attached to the SAME session S as A.
	b := dial(t, ts)
	send(t, b, map[string]any{"type": "hello", "token": "secret", "client_id": "devB"})
	readUntil(t, b, "hello_ok")
	send(t, b, map[string]any{"type": "attach", "name": sName})
	readUntil(t, b, "attached")

	// A renames S. Every connection attached to S (A and B) must be told directly.
	send(t, a, map[string]any{"type": "rename", "name": sName, "new_name": "renamed"})

	if m := readUntil(t, a, "renamed"); m["name"] != "renamed" || m["old"] != sName {
		t.Fatalf("initiator did not get renamed: %v", m)
	}
	if m := readUntil(t, b, "renamed"); m["name"] != "renamed" || m["old"] != sName {
		t.Fatalf("second device on the same session did not get renamed: %v", m)
	}

	// C, attached to a different session, must NOT receive `renamed`. Fence with an
	// explicit request whose reply proves C's stream drained past the rename: any
	// stray `renamed` would have been written to C before this reply.
	send(t, c, map[string]any{"type": "hosts"})
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := c.ReadJSON(&m); err != nil {
			t.Fatalf("fencing conn C: %v", err)
		}
		if m["type"] == "renamed" {
			t.Fatalf("connection attached to a different session received renamed: %v", m)
		}
		if m["type"] == "host_list" {
			break
		}
	}
}

// TestVoiceRename drives the "hey buddy rename to <name>" command end to end:
// attach to a session, rename it by voice, and confirm both the spoken
// confirmation and the refreshed session list carry the new name.
func TestVoiceRename(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")
	spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))

	// Rename the attached session by voice. Drain any buffered speech from the
	// attach flow; wait for both the refreshed session_list carrying the new name
	// and the spoken rename confirmation.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy rename to backend"})
	var gotSay, gotList bool
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !gotSay || !gotList {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for rename say+list (say=%v list=%v): %v", gotSay, gotList, err)
		}
		switch m["type"] {
		case "say":
			if strings.Contains(m["text"].(string), "backend") {
				gotSay = true
			}
		case "session_list":
			for _, s := range m["sessions"].([]any) {
				if s.(map[string]any)["name"] == "backend" {
					gotList = true
				}
			}
		}
	}
}

// TestSpokenErrorFeedback asserts a voice-reachable failure now speaks a
// friendly message alongside the machine-readable error, instead of failing
// silently. Renaming a session that doesn't exist trips rename_failed.
func TestSpokenErrorFeedback(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "rename", "name": "ghost", "new_name": "whatever"})
	// Both an `error` (rename_failed) and a spoken `say` must arrive, in either order.
	var gotErr, gotSay bool
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for !gotErr || !gotSay {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for error+say (err=%v say=%v): %v", gotErr, gotSay, err)
		}
		switch m["type"] {
		case "error":
			if m["code"] != "rename_failed" {
				t.Fatalf("unexpected error code: %v", m["code"])
			}
			gotErr = true
		case "say":
			gotSay = true
		}
	}
}

func TestDeleteSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")
	name := spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))

	// Delete it; the refreshed list must no longer contain it.
	send(t, ws, map[string]any{"type": "delete", "name": name})
	m := readUntil(t, ws, "session_list")
	sl, _ := m["sessions"].([]any)
	for _, s := range sl {
		if s.(map[string]any)["name"] == name {
			t.Fatalf("deleted session still present: %v", m["sessions"])
		}
	}
}

// TestDeleteDiscoveredIsPerSession verifies a delete removes ONLY the session
// matched by session_id, leaving other sessions in the same directory intact.
// Now that Discover shows every session individually (no dir collapse), each is
// separately deletable — deleting one must not take its dir-mates with it.
func TestDeleteDiscoveredIsPerSession(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	dir := t.TempDir()
	// Two records for the SAME dir, neither with a transcript on disk (just
	// spawned) — this exercises the no-transcript delete branch.
	if err := gw.store.Put(&session.Session{Name: "proj", Dir: dir, SessionID: "id-live"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.store.Put(&session.Session{Name: "proj-2", Dir: dir, SessionID: "id-keep"}); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Delete ONE session by id: only "proj" goes; its same-dir sibling survives.
	send(t, ws, map[string]any{"type": "delete_discovered", "session_id": "id-live"})
	m := readUntil(t, ws, "session_list")
	sl, _ := m["sessions"].([]any)
	for _, s := range sl {
		if s.(map[string]any)["name"] == "proj" {
			t.Fatalf("deleted session proj survived: %v", m["sessions"])
		}
	}
	if gw.store.Get("proj") != nil {
		t.Fatal("deleted record proj still in store")
	}
	if gw.store.Get("proj-2") == nil {
		t.Fatal("sibling record proj-2 was wrongly deleted with proj")
	}
}

// TestDiscoverShowsEverySessionInADir verifies discovery no longer collapses a
// directory to a single row: multiple registered sessions sharing a dir each
// appear as their own entry, keyed by their own session_id (so none is hidden).
func TestDiscoverShowsEverySessionInADir(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	dir := t.TempDir()
	if err := gw.store.Put(&session.Session{Name: "proj", Dir: dir, SessionID: "id-a"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.store.Put(&session.Session{Name: "proj-2", Dir: dir, SessionID: "id-b"}); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "discover"})
	m := readUntil(t, ws, "discovered")
	got := map[string]string{} // name -> session_id
	for _, s := range m["sessions"].([]any) {
		sm := s.(map[string]any)
		got[sm["name"].(string)], _ = sm["session_id"].(string)
	}
	if got["proj"] != "id-a" {
		t.Errorf("proj row missing or wrong id: %v", m["sessions"])
	}
	if got["proj-2"] != "id-b" {
		t.Errorf("proj-2 row missing or wrong id (dir was collapsed?): %v", m["sessions"])
	}
}

func TestListEmptyThenAfterSpawn(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy list sessions"})
	m := readUntil(t, ws, "session_list")
	if sl, ok := m["sessions"].([]any); !ok || len(sl) != 0 {
		t.Fatalf("expected empty session list, got %v", m["sessions"])
	}
}

func TestReconnectResumesDialog(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "client_id": "c2"})
	readUntil(t, ws, "hello_ok")
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_path
	ws.Close()
	time.Sleep(100 * time.Millisecond)

	// Reconnect -> dialog resumes at await_path.
	ws2 := dial(t, ts)
	send(t, ws2, map[string]any{"type": "hello", "token": "secret", "client_id": "c2"})
	readUntil(t, ws2, "hello_ok")
	if got := readUntil(t, ws2, "dialog")["state"]; got != "await_path" {
		t.Fatalf("resume dialog state = %v, want await_path", got)
	}
}

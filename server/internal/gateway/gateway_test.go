package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// fakeClaude writes a script that mimics `claude -p --output-format stream-json`:
// it emits an init event and a success result, ignoring all arguments.
func fakeClaude(t *testing.T, reply string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeclaude.sh")
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","result":"` + reply + `","session_id":"fake"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeClaudeCapture is fakeClaude that also appends each invocation's arguments
// (the prompt among them) to capturePath, delimited by a marker, so a test can
// assert what text actually reached claude on each turn.
func fakeClaudeCapture(t *testing.T, reply, capturePath string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeclaude.sh")
	script := "#!/bin/sh\n" +
		`printf '===RECORD===\n' >> ` + capturePath + "\n" +
		`printf '%s\n' "$@" >> ` + capturePath + "\n" +
		`echo '{"type":"system","subtype":"init"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","result":"` + reply + `","session_id":"fake"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeSTT is a stub Transcriber that returns a fixed string and records the WAV
// it was handed.
type fakeSTT struct {
	text   string
	gotWAV []byte
}

func (f *fakeSTT) Transcribe(_ context.Context, wav []byte, _ transcribe.Options) (string, error) {
	f.gotWAV = wav
	return f.text, nil
}

func newTestServer(t *testing.T, stt transcribe.Transcriber) (*httptest.Server, string) {
	t.Helper()
	ts, root, _ := newTestServerGW(t, stt)
	return ts, root
}

// newTestServerGW is newTestServer but also returns the gateway, for tests that
// need to observe server-level state (e.g. RestartRequested).
func newTestServerGW(t *testing.T, stt transcribe.Transcriber) (*httptest.Server, string, *Server) {
	t.Helper()
	// Use an all-lowercase temp root: STT output is lowercase, so a root with
	// capitals (as t.TempDir produces) can't be matched from a spoken path.
	root, err := os.MkdirTemp("", "spawner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	cfg := &config.Config{
		AuthToken:  "secret",
		SpawnRoots: []string{root},
		StatePath:  filepath.Join(t.TempDir(), "sessions.json"),
		ClaudeBin:  "claude",
	}
	store, err := session.OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	driver := session.NewDriver()
	driver.HostBin(fakeClaude(t, "pong"))
	driver.Bypass = false
	gw := New(cfg, store, driver, tmux.NewManager(), stt, projects.New(cfg.SpawnRoots))
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWS))
	t.Cleanup(ts.Close)
	return ts, root, gw
}

func dial(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return ws
}

func send(t *testing.T, ws *websocket.Conn, v map[string]any) {
	t.Helper()
	if err := ws.WriteJSON(v); err != nil {
		t.Fatal(err)
	}
}

// readUntil reads messages until one with the given type arrives (or times out).
func readUntil(t *testing.T, ws *websocket.Conn, typ string) map[string]any {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("waiting for %q: %v", typ, err)
		}
		if m["type"] == typ {
			return m
		}
	}
}

func TestAuthRejectsBadToken(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "wrong"})
	m := readUntil(t, ws, "error")
	if m["code"] != "unauthorized" {
		t.Fatalf("expected unauthorized, got %v", m)
	}
}

func TestSpawnDialogAndDictation(t *testing.T) {
	ts, root := newTestServer(t, nil)
	// An existing folder under the root to navigate into.
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)

	// Handshake.
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// "hey buddy, spawn a new session" -> asks for a root.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy, spawn a new session"})
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_root" {
		t.Fatalf("expected await_root, got %v", d["state"])
	}

	// Pick the root by its name -> it has children, so it asks which folder.
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	d = readUntil(t, ws, "dialog")
	if d["state"] != "await_child" {
		t.Fatalf("expected await_child, got %v", d["state"])
	}

	// Navigate into myproj -> it's a leaf, so it moves to the attach question.
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	d = readUntil(t, ws, "dialog")
	if d["state"] != "await_attach" {
		t.Fatalf("expected await_attach, got %v", d["state"])
	}

	// "yes" -> attach.
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	a := readUntil(t, ws, "attached")
	name, _ := a["name"].(string)
	if name == "" {
		t.Fatalf("expected a session name, got empty")
	}

	// Dictate -> fake claude returns "pong".
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong"})
	out := readUntil(t, ws, "output")
	if out["text"] != "pong" {
		t.Fatalf("expected dictation output 'pong', got %v", out["text"])
	}
}

// TestMultiDeviceLiveFanout: two connections attached to the same session both
// receive a dictated turn's reply live (not just the one that dictated).
func TestMultiDeviceLiveFanout(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Device A: spawn + attach.
	a := dial(t, ts)
	send(t, a, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, a, "hello_ok")
	send(t, a, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, a, "dialog") // await_root
	send(t, a, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, a, "dialog") // await_child
	send(t, a, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, a, "dialog") // await_attach
	send(t, a, map[string]any{"type": "utterance", "text": "yes"})
	name := readUntil(t, a, "attached")["name"].(string)

	// Device B: attach to the SAME session (passive watcher).
	b := dial(t, ts)
	send(t, b, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, b, "hello_ok")
	send(t, b, map[string]any{"type": "attach", "name": name})
	readUntil(t, b, "attached") // ensures B's sink is registered before A dictates

	// Device A dictates -> both A and B must receive the reply live.
	send(t, a, map[string]any{"type": "utterance", "text": "say pong"})
	if out := readUntil(t, a, "output"); out["text"] != "pong" {
		t.Fatalf("device A: expected 'pong', got %v", out["text"])
	}
	if out := readUntil(t, b, "output"); out["text"] != "pong" {
		t.Fatalf("device B (fan-out): expected 'pong', got %v", out["text"])
	}
}

// TestJobBuffersWhenSinkFails: if a turn finishes while its only sink's write
// fails (a dropped socket the server hasn't noticed yet), the result must be
// buffered (delivered=false) so it's delivered on reconnect — not treated as
// delivered and lost, which left the app stuck on "running the command".
func TestJobBuffersWhenSinkFails(t *testing.T) {
	dead := &conn{}
	j := &sessionJob{running: true, sinks: map[*conn]func(any) bool{
		dead: func(any) bool { return false }, // simulate a failed write
	}}
	j.finish(map[string]any{"type": "output", "text": "done"})
	if j.delivered {
		t.Fatal("delivered should be false when the only sink's write failed")
	}
	if j.final == nil {
		t.Fatal("result must be buffered for delivery on reconnect")
	}

	// A live sink attaching (reconnect) then gets the buffered result and frees it.
	var got any
	live := &conn{}
	j.sinks[live] = func(v any) bool { got = v; return true }
	// mimic bindJob's deliver-buffered branch
	if !j.delivered && j.final != nil {
		if j.sinks[live](j.final) {
			j.delivered = true
			j.final = nil
		}
	}
	if got == nil || j.final != nil || !j.delivered {
		t.Fatalf("buffered result not delivered on reconnect: got=%v final=%v delivered=%v", got, j.final, j.delivered)
	}
}

func TestInflightTrackerRecoversPrior(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inflight.json")
	// Fresh: nothing prior. Mark a turn in-flight, then "die".
	t1, prior1 := newInflightTracker(path)
	if len(prior1) != 0 {
		t.Fatalf("fresh tracker should have no prior, got %v", prior1)
	}
	t1.add("mysess")
	// Restart: the interrupted session is recovered, and the file resets.
	_, prior2 := newInflightTracker(path)
	if !prior2["mysess"] {
		t.Fatalf("restart should recover mysess, got %v", prior2)
	}
	// Next restart: already consumed/reset — nothing left.
	_, prior3 := newInflightTracker(path)
	if len(prior3) != 0 {
		t.Fatalf("should be empty after recovery, got %v", prior3)
	}
}

func TestSpawnCreatesNewFolder(t *testing.T) {
	ts, root := newTestServer(t, nil)
	// Root needs a child so it prompts (await_child) rather than using itself.
	if err := os.MkdirAll(filepath.Join(root, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child

	// A name that matches nothing -> offer to create it.
	send(t, ws, map[string]any{"type": "utterance", "text": "brandnew"})
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_create" {
		t.Fatalf("expected await_create, got %v", d["state"])
	}
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	readUntil(t, ws, "dialog") // await_attach
	if _, err := os.Stat(filepath.Join(root, "brandnew")); err != nil {
		t.Fatalf("new folder not created: %v", err)
	}
}

// newSandboxTestServer builds a gateway with a sandbox image configured, so the
// spawn dialog gains the host-vs-sandbox target step.
func newSandboxTestServer(t *testing.T) (*httptest.Server, string, *Server) {
	t.Helper()
	root, err := os.MkdirTemp("", "spawner")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	cfg := &config.Config{
		AuthToken:    "secret",
		SpawnRoots:   []string{root},
		StatePath:    filepath.Join(t.TempDir(), "sessions.json"),
		ClaudeBin:    "claude",
		SandboxImage: "spawner-sandbox:latest",
	}
	store, err := session.OpenStore(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	driver := session.NewDriver()
	driver.HostBin(fakeClaude(t, "pong"))
	gw := New(cfg, store, driver, tmux.NewManager(), nil, projects.New(cfg.SpawnRoots))
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWS))
	t.Cleanup(ts.Close)
	return ts, root, gw
}

// TestSpawnAsksTargetWhenSandboxConfigured: with a sandbox image set, spawning
// inserts an await_target step and the chosen target is persisted on the record.
func TestSpawnAsksTargetWhenSandboxConfigured(t *testing.T) {
	ts, root, gw := newSandboxTestServer(t)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})

	// The new step: host or sandbox?
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_target" {
		t.Fatalf("expected await_target, got %v", d["state"])
	}
	send(t, ws, map[string]any{"type": "utterance", "text": "in a sandbox"})
	d = readUntil(t, ws, "dialog")
	if d["state"] != "await_attach" {
		t.Fatalf("expected await_attach after target, got %v", d["state"])
	}
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	a := readUntil(t, ws, "attached")
	name, _ := a["name"].(string)

	if rec := gw.store.Get(name); rec == nil {
		t.Fatalf("session %q not persisted", name)
	} else if rec.Target != session.TargetSandbox {
		t.Errorf("Target = %q, want %q", rec.Target, session.TargetSandbox)
	}
}

func TestAudioPathTranscribesAndDispatches(t *testing.T) {
	stt := &fakeSTT{text: "hey buddy spawn a new session"}
	ts, _ := newTestServer(t, stt)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Simulate an utterance: wake, a couple PCM frames, audio_end.
	send(t, ws, map[string]any{"type": "wake"})
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "audio_end"})

	// The server should echo the transcript, then start the spawn dialog from it.
	tr := readUntil(t, ws, "transcript")
	if tr["text"] != stt.text {
		t.Fatalf("transcript = %v, want %q", tr["text"], stt.text)
	}
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_root" {
		t.Fatalf("expected await_root from transcribed utterance, got %v", d["state"])
	}
	// And the transcriber must have received a real WAV (RIFF header).
	if len(stt.gotWAV) < 44 || string(stt.gotWAV[:4]) != "RIFF" {
		t.Fatalf("transcriber did not get a WAV; got %d bytes", len(stt.gotWAV))
	}
}

func TestAudioRejectedWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t, nil) // no transcriber
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "wake"})
	m := readUntil(t, ws, "error")
	if m["code"] != "not_implemented" {
		t.Fatalf("expected not_implemented, got %v", m)
	}
}

func TestRenameSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")
	// Spawn + attach to create a session record.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, ws, "dialog") // await_attach
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	old := readUntil(t, ws, "attached")["name"].(string)

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
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	readUntil(t, ws, "attached")

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
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	name := readUntil(t, ws, "attached")["name"].(string)

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

// TestDeleteDiscoveredClearsSameDirGhosts verifies a delete removes EVERY
// registry record for the session's directory, not just the one matched by
// session_id. A shadowed same-dir duplicate (the sidebar collapses same-dir
// rows to one) must not survive as a ghost — it would still own a name and
// block renaming another session onto it.
func TestDeleteDiscoveredClearsSameDirGhosts(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	dir := t.TempDir()
	// Two records for the SAME dir, neither with a transcript on disk (just
	// spawned) — this exercises the no-transcript delete branch.
	if err := gw.store.Put(&session.Session{Name: "proj", Dir: dir, SessionID: "id-live"}); err != nil {
		t.Fatal(err)
	}
	if err := gw.store.Put(&session.Session{Name: "proj-2", Dir: dir, SessionID: "id-ghost"}); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Delete via one session_id; BOTH same-dir records must be gone.
	send(t, ws, map[string]any{"type": "delete_discovered", "session_id": "id-live"})
	m := readUntil(t, ws, "session_list")
	sl, _ := m["sessions"].([]any)
	for _, s := range sl {
		if n := s.(map[string]any)["name"]; n == "proj" || n == "proj-2" {
			t.Fatalf("same-dir record survived delete: %v", m["sessions"])
		}
	}
	if gw.store.Get("proj-2") != nil {
		t.Fatal("ghost record proj-2 still in store after delete")
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
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child
	ws.Close()
	time.Sleep(100 * time.Millisecond)

	// Reconnect -> dialog resumes at await_child.
	ws2 := dial(t, ts)
	send(t, ws2, map[string]any{"type": "hello", "token": "secret", "client_id": "c2"})
	readUntil(t, ws2, "hello_ok")
	if got := readUntil(t, ws2, "dialog")["state"]; got != "await_child" {
		t.Fatalf("resume dialog state = %v, want await_child", got)
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

// TestInteractiveAskInstructionPrimedOnce verifies the interactive-mode ask
// instruction is appended to the FIRST turn of a session's context and omitted
// on later turns (Claude retains it via --resume), so it doesn't burn tokens.
func TestInteractiveAskInstructionPrimedOnce(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(t.TempDir(), "prompts.txt")
	gw.driver.HostBin(fakeClaudeCapture(t, "ok", capPath))

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "interactive": true})
	readUntil(t, ws, "hello_ok")

	// Spawn + attach a session (fake claude runs only on dictation, not spawn).
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy, spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	readUntil(t, ws, "attached")

	send(t, ws, map[string]any{"type": "utterance", "text": "first turn"})
	readUntil(t, ws, "output")
	send(t, ws, map[string]any{"type": "utterance", "text": "second turn"})
	readUntil(t, ws, "output")

	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	for _, r := range strings.Split(string(data), "===RECORD===") {
		if strings.TrimSpace(r) != "" {
			prompts = append(prompts, r)
		}
	}
	if len(prompts) != 2 {
		t.Fatalf("expected 2 claude invocations, got %d: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[0], "[Interactive mode]") {
		t.Fatalf("first interactive turn should carry the ask instruction, got %q", prompts[0])
	}
	if strings.Contains(prompts[1], "[Interactive mode]") {
		t.Fatalf("second turn must not re-send the ask instruction, got %q", prompts[1])
	}
}

// TestCompressSummarizesAndSeedsNextTurn: `compress` runs a summary turn, rotates
// the session_id (old id retired to PriorIDs), and prepends the summary to the
// NEXT dictation so context is carried forward condensed rather than dropped.
func TestCompressSummarizesAndSeedsNextTurn(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(t.TempDir(), "prompts.txt")
	gw.driver.HostBin(fakeClaudeCapture(t, "recap-blob", capPath))

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy, spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "myproj"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	name := readUntil(t, ws, "attached")["name"].(string)

	// First real turn establishes context.
	send(t, ws, map[string]any{"type": "utterance", "text": "first turn"})
	readUntil(t, ws, "output")
	origID := gw.store.Get(name).SessionID

	// Compress: a background summary turn, then a rotation. The confirming say lands
	// after the summary completes.
	send(t, ws, map[string]any{"type": "compress"})
	if m := readUntil(t, ws, "say"); !strings.Contains(m["text"].(string), "compressed") {
		t.Fatalf("compress say = %v, want a 'compressed' confirmation", m["text"])
	}
	rec := gw.store.Get(name)
	if len(rec.PriorIDs) != 1 || rec.PriorIDs[0] != origID {
		t.Fatalf("expected the original session_id retired to PriorIDs, got %v (orig %q)", rec.PriorIDs, origID)
	}
	if rec.SessionID == origID {
		t.Fatalf("compress must rotate to a fresh session_id, still %q", rec.SessionID)
	}
	if rec.PendingSeed != "recap-blob" {
		t.Fatalf("compress should stash the summary as PendingSeed, got %q", rec.PendingSeed)
	}
	if rec.Started {
		t.Fatal("rotated session should be un-Started until the next dictation")
	}

	// Next dictation carries the summary forward and clears the pending seed.
	send(t, ws, map[string]any{"type": "utterance", "text": "next turn"})
	readUntil(t, ws, "output")
	if seed := gw.store.Get(name).PendingSeed; seed != "" {
		t.Fatalf("PendingSeed should be cleared after the seeded turn, got %q", seed)
	}

	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	for _, r := range strings.Split(string(data), "===RECORD===") {
		if strings.TrimSpace(r) != "" {
			prompts = append(prompts, r)
		}
	}
	if len(prompts) != 3 {
		t.Fatalf("expected 3 claude invocations (turn, summary, seeded turn), got %d: %q", len(prompts), prompts)
	}
	if !strings.Contains(prompts[1], "Summarize our conversation") {
		t.Fatalf("second invocation should be the summary request, got %q", prompts[1])
	}
	if !strings.Contains(prompts[2], "recap-blob") || !strings.Contains(prompts[2], "next turn") {
		t.Fatalf("third invocation should prepend the summary to the dictation, got %q", prompts[2])
	}
}

func TestRestartBroadcastsAndSignals(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)

	// Two clients: the one that asks and a bystander. Both should hear the `say`.
	asker := dial(t, ts)
	send(t, asker, map[string]any{"type": "hello", "token": "secret", "client_id": "a"})
	readUntil(t, asker, "hello_ok")
	other := dial(t, ts)
	send(t, other, map[string]any{"type": "hello", "token": "secret", "client_id": "b"})
	readUntil(t, other, "hello_ok")

	send(t, asker, map[string]any{"type": "restart"})
	if m := readUntil(t, asker, "say"); !strings.Contains(m["text"].(string), "restarting") {
		t.Fatalf("asker say = %v, want a restarting notice", m["text"])
	}
	if m := readUntil(t, other, "say"); !strings.Contains(m["text"].(string), "restarting") {
		t.Fatalf("bystander say = %v, want a restarting notice", m["text"])
	}

	// main() would now exit-for-relaunch: the restart channel must be closed.
	select {
	case <-gw.RestartRequested():
	case <-time.After(2 * time.Second):
		t.Fatal("RestartRequested was not signaled")
	}

	// Idempotent: a second request must not panic on a double close.
	gw.RequestRestart()
}

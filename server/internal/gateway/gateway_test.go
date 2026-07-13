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
	hosts, err := session.OpenHostStore(filepath.Join(t.TempDir(), "hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := session.OpenIdentityStore(filepath.Join(t.TempDir(), "identities.json"), filepath.Join(t.TempDir(), "keys"))
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, store, hosts, ids, nil, driver, tmux.NewManager(), stt, nil, projects.New(cfg.SpawnRoots))
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

func TestHostCRUD(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// A fresh registry seeds the loopback host; remove it so the rest of this test
	// works against an empty registry.
	send(t, ws, map[string]any{"type": "hosts"})
	if hs := readUntil(t, ws, "host_list")["hosts"].([]any); len(hs) != 1 || hs[0].(map[string]any)["name"] != "localhost" {
		t.Fatalf("fresh registry should seed localhost, got %v", hs)
	}
	send(t, ws, map[string]any{"type": "host_delete", "name": "localhost"})
	if hs := readUntil(t, ws, "host_list")["hosts"].([]any); len(hs) != 0 {
		t.Fatalf("registry should be empty after removing seed, got %v", hs)
	}

	// Add a host → broadcast list with it.
	send(t, ws, map[string]any{"type": "host_put", "host": map[string]any{"name": "work", "address": "100.64.0.7"}})
	hs := readUntil(t, ws, "host_list")["hosts"].([]any)
	if len(hs) != 1 {
		t.Fatalf("want 1 host, got %v", hs)
	}
	if h := hs[0].(map[string]any); h["name"] != "work" || h["address"] != "100.64.0.7" {
		t.Fatalf("unexpected host: %v", h)
	}

	// A nameless host is rejected.
	send(t, ws, map[string]any{"type": "host_put", "host": map[string]any{"address": "x"}})
	if e := readUntil(t, ws, "error"); e["code"] != "bad_host" {
		t.Fatalf("want bad_host, got %v", e)
	}

	// Delete → broadcast empty list.
	send(t, ws, map[string]any{"type": "host_delete", "name": "work"})
	if hs := readUntil(t, ws, "host_list")["hosts"].([]any); len(hs) != 0 {
		t.Fatalf("registry should be empty after delete, got %v", hs)
	}
}

func TestIdentityCRUD(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Fresh registry is empty.
	send(t, ws, map[string]any{"type": "identities"})
	if ids := readUntil(t, ws, "identity_list")["identities"].([]any); len(ids) != 0 {
		t.Fatalf("fresh identity registry should be empty, got %v", ids)
	}

	// Create → broadcast list with a public key (never a private key or password).
	send(t, ws, map[string]any{"type": "identity_create", "name": "work", "user": "bam", "password": "s3cret"})
	ids := readUntil(t, ws, "identity_list")["identities"].([]any)
	if len(ids) != 1 {
		t.Fatalf("want 1 identity, got %v", ids)
	}
	id := ids[0].(map[string]any)
	if id["name"] != "work" || id["user"] != "bam" || !strings.Contains(id["public_key"].(string), "ssh-ed25519") {
		t.Fatalf("unexpected identity: %v", id)
	}
	if id["has_password"] != true {
		t.Fatalf("has_password should be reported true: %v", id)
	}
	if _, leaked := id["private_key"]; leaked {
		t.Fatalf("private key must never be sent: %v", id)
	}
	if _, leaked := id["password"]; leaked {
		t.Fatalf("password must never be sent: %v", id)
	}

	// A username is required.
	send(t, ws, map[string]any{"type": "identity_create", "name": "nouser"})
	if e := readUntil(t, ws, "error"); e["code"] != "bad_identity" {
		t.Fatalf("want bad_identity for missing user, got %v", e)
	}

	// A duplicate name is rejected.
	send(t, ws, map[string]any{"type": "identity_create", "name": "work", "user": "bam"})
	if e := readUntil(t, ws, "error"); e["code"] != "bad_identity" {
		t.Fatalf("want bad_identity, got %v", e)
	}

	// Delete → broadcast empty list.
	send(t, ws, map[string]any{"type": "identity_delete", "name": "work"})
	if ids := readUntil(t, ws, "identity_list")["identities"].([]any); len(ids) != 0 {
		t.Fatalf("registry should be empty after delete, got %v", ids)
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

// TestSpawnFuzzyMatchConfirm: a spoken folder name that only fuzzy-matches an
// existing folder ("mail" -> "mail_play") prompts a yes/no confirmation before
// committing, so a misheard name doesn't silently attach to the wrong project.
func TestSpawnFuzzyMatchConfirm(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "mail_play"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child

	// "mail" fuzzy-matches "mail_play" -> confirm rather than attach.
	send(t, ws, map[string]any{"type": "utterance", "text": "mail"})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_confirm" {
		t.Fatalf("expected await_confirm, got %v", d["state"])
	}

	// "no" backs up to the folder list.
	send(t, ws, map[string]any{"type": "utterance", "text": "no"})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_child" {
		t.Fatalf("expected await_child after decline, got %v", d["state"])
	}

	// Try again and confirm -> proceeds to the attach question.
	send(t, ws, map[string]any{"type": "utterance", "text": "mail"})
	readUntil(t, ws, "dialog") // await_confirm
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_attach" {
		t.Fatalf("expected await_attach after confirm, got %v", d["state"])
	}
}

// TestSpawnExactMatchNoConfirm: an exact folder name skips the confirmation and
// goes straight to attach (the confirm step is only for fuzzy hits).
func TestSpawnExactMatchNoConfirm(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "mail_play"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_root
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Base(root)})
	readUntil(t, ws, "dialog") // await_child
	send(t, ws, map[string]any{"type": "utterance", "text": "mail play"})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_attach" {
		t.Fatalf("expected await_attach for exact match, got %v", d["state"])
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
	hosts, err := session.OpenHostStore(filepath.Join(t.TempDir(), "hosts.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids, err := session.OpenIdentityStore(filepath.Join(t.TempDir(), "identities.json"), filepath.Join(t.TempDir(), "keys"))
	if err != nil {
		t.Fatal(err)
	}
	gw := New(cfg, store, hosts, ids, nil, driver, tmux.NewManager(), nil, nil, projects.New(cfg.SpawnRoots))
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWS))
	t.Cleanup(ts.Close)
	return ts, root, gw
}

// fakeSandbox records container-lifecycle calls, standing in for the real
// rootless-runtime executor so tests can assert the spawn/delete hooks fire
// without a container runtime.
type fakeSandbox struct {
	ensured map[string]string // container name -> dir
	removed []string
}

func (f *fakeSandbox) Start(context.Context, *session.Session, string, []string) (session.Proc, error) {
	return nil, nil // turns aren't exercised in the lifecycle test
}
func (f *fakeSandbox) Ensure(_ context.Context, name, dir string) error {
	if f.ensured == nil {
		f.ensured = map[string]string{}
	}
	f.ensured[name] = dir
	return nil
}
func (f *fakeSandbox) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

// TestSpawnAsksTargetWhenSandboxConfigured: with a sandbox image set, spawning
// inserts an await_target step, the chosen target is persisted on the record, and
// the persistent container is created at spawn and removed on delete.
func TestSpawnAsksTargetWhenSandboxConfigured(t *testing.T) {
	ts, root, gw := newSandboxTestServer(t)
	fake := &fakeSandbox{}
	gw.driver.Execs[session.TargetSandbox] = fake
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

	rec := gw.store.Get(name)
	if rec == nil {
		t.Fatalf("session %q not persisted", name)
	}
	if rec.Target != session.TargetSandbox {
		t.Errorf("Target = %q, want %q", rec.Target, session.TargetSandbox)
	}
	// Persistent container created at spawn, bound to the session's dir.
	if rec.Container == "" {
		t.Fatal("sandbox session has no container name")
	}
	if got := fake.ensured[rec.Container]; got != rec.Dir {
		t.Errorf("Ensure(%q) dir = %q, want %q", rec.Container, got, rec.Dir)
	}

	// The sidebar's `discovered` feed must carry target=sandbox for this session so
	// the app can badge it.
	send(t, ws, map[string]any{"type": "discover"})
	disc := readUntil(t, ws, "discovered")
	sessions, _ := disc["sessions"].([]any)
	var found map[string]any
	for _, s := range sessions {
		if m, _ := s.(map[string]any); m["name"] == name {
			found = m
		}
	}
	if found == nil {
		t.Fatalf("session %q missing from discovered feed", name)
	}
	if found["target"] != "sandbox" {
		t.Errorf("discovered target = %v, want sandbox", found["target"])
	}

	// Deleting the session must destroy its container.
	send(t, ws, map[string]any{"type": "delete", "name": name})
	readUntil(t, ws, "session_list")
	if len(fake.removed) != 1 || fake.removed[0] != rec.Container {
		t.Errorf("Remove calls = %v, want [%q]", fake.removed, rec.Container)
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

func TestAudioUnknownCodecRejected(t *testing.T) {
	stt := &fakeSTT{text: "ignored"}
	ts, _ := newTestServer(t, stt)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "wake", "codec": "mp3"})
	m := readUntil(t, ws, "error")
	if m["code"] != "bad_message" {
		t.Fatalf("expected bad_message for unknown codec, got %v", m)
	}

	// The rejected wake must not have started collecting: audio_end with no
	// accepted wake is a no-op, not a transcription of stray frames.
	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	send(t, ws, map[string]any{"type": "audio_end"})
	send(t, ws, map[string]any{"type": "wake", "codec": "mp3"}) // fence: errors after audio_end ran
	readUntil(t, ws, "error")
	if len(stt.gotWAV) != 0 {
		t.Fatalf("transcriber ran on %d bytes after a rejected wake", len(stt.gotWAV))
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

	// The job hub is keyed by session_id (stable across the rename), so a turn
	// dictated after the rename must still fan out to this attached connection —
	// no hub re-keying required. Regression guard for dropping renameJob.
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong"})
	if out := readUntil(t, ws, "output"); out["text"] != "pong" {
		t.Fatalf("no turn output after rename (hub lost across rename?): %v", out["text"])
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

// TestAdoptStaleIDReusesLiveSession verifies that adopting a discovered session
// whose session_id is stale (the folder already has a live local session under a
// different id) attaches to the live session instead of registering a phantom
// "-2" duplicate. This reproduces the fresh-open-while-offline bug: the app shows
// a cached discovered row for a since-superseded session and the user taps it.
func TestAdoptStaleIDReusesLiveSession(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	dir := t.TempDir()
	// The live session that currently owns the folder.
	if err := gw.store.Put(&session.Session{Name: "proj", Dir: dir, SessionID: "id-live", Host: session.LocalHost}); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Adopt a DIFFERENT (stale) id for the same dir — the cached discovered entry.
	send(t, ws, map[string]any{"type": "adopt", "session_id": "id-stale", "path": dir})
	a := readUntil(t, ws, "attached")
	if name, _ := a["name"].(string); name != "proj" {
		t.Fatalf("adopt should attach to the live session, got %q", name)
	}
	if gw.store.Get("proj-2") != nil {
		t.Fatal("adopt minted a phantom proj-2 duplicate for the stale id")
	}
	if gw.store.GetBySessionID("id-stale") != nil {
		t.Fatal("stale id was registered instead of reusing the live session")
	}
}

// TestSpawnAtReusesExistingSession verifies opening a folder that already has a
// session attaches to it instead of minting a same-folder "-2" duplicate.
func TestSpawnAtReusesExistingSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Open the folder twice; the second open must reuse the same session (same
	// name), not mint a "proj-2" duplicate.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir})
	first := readUntil(t, ws, "attached")["name"].(string)
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir})
	second := readUntil(t, ws, "attached")["name"].(string)

	if second != first {
		t.Fatalf("second open made a new session %q (want reuse of %q)", second, first)
	}
}

// TestSpawnAtDifferentHostMakesNewSession: the same folder on a different host is
// a distinct session — the dedup matches directory AND host, so picking a remote
// host must not reuse the localhost session sitting at the same path.
func TestSpawnAtDifferentHostMakesNewSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Default (loopback) spawn.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir})
	local := readUntil(t, ws, "attached")["name"].(string)

	// Same folder, explicit remote host → a brand-new session, not a reuse.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "host_name": "remote"})
	remote := readUntil(t, ws, "attached")["name"].(string)
	if remote == local {
		t.Fatalf("remote-host spawn reused the localhost session %q (host pick dropped)", local)
	}

	// Re-picking the same host reuses that session rather than minting a third.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "host_name": "remote"})
	if again := readUntil(t, ws, "attached")["name"].(string); again != remote {
		t.Fatalf("second remote spawn made a new session %q (want reuse of %q)", again, remote)
	}
}

// TestSpawnAtCreatesNewFolder: spawn_at with create=true makes a brand-new
// directory before attaching, so the picker can start a project in a folder that
// doesn't exist yet.
func TestSpawnAtCreatesNewFolder(t *testing.T) {
	ts, root := newTestServer(t, nil)
	dir := filepath.Join(root, "brand-new-proj")
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "create": true})
	if name := readUntil(t, ws, "attached")["name"].(string); name == "" {
		t.Fatal("expected a session name after creating the folder")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("folder %q was not created: %v", dir, err)
	}

	// Creating the same folder again fails (it exists now) rather than clobbering.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "create": true})
	if m := readUntil(t, ws, "error"); m["code"] != "bad_path" {
		t.Fatalf("expected bad_path re-creating an existing folder, got %v", m)
	}
}

// TestSpawnAtRejectsRelativePath: the visual picker walks the target host's whole
// filesystem (no spawn-root jail), but a path must still be absolute — a relative
// one is meaningless without a host-side cwd and is rejected.
func TestSpawnAtRejectsRelativePath(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "spawn_at", "path": "relative/path", "create": true})
	if m := readUntil(t, ws, "error"); m["code"] != "bad_path" {
		t.Fatalf("expected bad_path for a relative spawn path, got %v", m)
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

func TestRestartTriggersRebuildAndBroadcasts(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	// A restart command that leaves a sentinel file, so the test can confirm it
	// actually fired (Driver.Restart runs it detached and returns immediately).
	marker := filepath.Join(t.TempDir(), "restarted")
	gw.driver.RestartCmd = "touch " + marker

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
	// The detached command runs asynchronously; poll briefly for its side effect.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restart command did not run (sentinel %q never appeared)", marker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRestartFailsWithoutCmd: with no SPAWNER_RESTART_CMD configured, restart
// reports a failure instead of silently doing nothing.
func TestRestartFailsWithoutCmd(t *testing.T) {
	ts, _, _ := newTestServerGW(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "restart"})
	if m := readUntil(t, ws, "error"); !strings.Contains(m["code"].(string), "restart_failed") {
		t.Fatalf("error code = %v, want restart_failed", m["code"])
	}
}

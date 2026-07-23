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

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/detect"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/spoken"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
	"github.com/gorilla/websocket"
)

// fakeClaude writes a script that mimics `claude -p --output-format stream-json`:
// it emits an init event and a success result, ignoring all arguments.
// testTokens builds an in-temp spoken-token store seeded with the built-in
// defaults, so wake/end matching behaves like production in gateway tests.
func testTokens(t *testing.T) *session.SpokenTokenStore {
	t.Helper()
	ts, err := session.OpenSpokenTokenStore(filepath.Join(t.TempDir(), "spoken_tokens.json"),
		spoken.DefaultTokens(command.DefaultWakePhrases(), detect.WakeModel, detect.EndModel))
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

// tokensSeed builds an in-temp spoken-token store from an explicit seed, for tests
// that need a specific set of wake/end/speak tokens.
func tokensSeed(t *testing.T, seed []*spoken.Token) *session.SpokenTokenStore {
	t.Helper()
	ts, err := session.OpenSpokenTokenStore(filepath.Join(t.TempDir(), "spoken_tokens.json"), seed)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

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
		AuthToken: "secret",
		StatePath: filepath.Join(t.TempDir(), "sessions.json"),
		ClaudeBin: "claude",
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
	gw := New(cfg, store, hosts, ids, testTokens(t), nil, driver, tmux.NewManager(), stt, nil)
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

// spawnAttachVoice drives the new voice spawn flow to create and attach a session
// in dir (which must already exist on the local FS), returning its name. Assumes
// the test server has no sandbox configured (so no await_target step).
func spawnAttachVoice(t *testing.T, ws *websocket.Conn, dir string) string {
	t.Helper()
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_path
	send(t, ws, map[string]any{"type": "utterance", "text": dir})
	readUntil(t, ws, "dialog") // await_attach
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	return readUntil(t, ws, "attached")["name"].(string)
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
	gw := New(cfg, store, hosts, ids, testTokens(t), nil, driver, tmux.NewManager(), nil, nil)
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
func (f *fakeSandbox) Ensure(_ context.Context, sess *session.Session) error {
	if f.ensured == nil {
		f.ensured = map[string]string{}
	}
	f.ensured[sess.Container] = sess.Dir
	return nil

}
func (f *fakeSandbox) Remove(_ context.Context, name string) error {
	f.removed = append(f.removed, name)
	return nil
}

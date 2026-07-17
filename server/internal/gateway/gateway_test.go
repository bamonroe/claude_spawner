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
	gw := New(cfg, store, hosts, ids, nil, driver, tmux.NewManager(), stt, nil)
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

func TestProfilesAdvertisedOnConnect(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	reg, err := session.NewProfileRegistry(
		session.ExecProfile{Name: "host", Target: session.TargetHost, Default: true},
		session.ExecProfile{Name: "ollama", Target: session.TargetSandbox},
	)
	if err != nil {
		t.Fatal(err)
	}
	gw.driver.Profiles = reg

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "client_id": "profiles"})
	readUntil(t, ws, "hello_ok")
	readUntil(t, ws, "agents")
	msg := readUntil(t, ws, "profiles")

	items, ok := msg["profiles"].([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("profiles = %#v, want two entries", msg["profiles"])
	}
	names := []string{}
	for _, item := range items {
		m := item.(map[string]any)
		names = append(names, m["name"].(string))
	}
	if strings.Join(names, ",") != "host,ollama" {
		t.Fatalf("profile names = %v", names)
	}
	if msg["default"] != "host" {
		t.Fatalf("default = %#v", msg["default"])
	}
}

// TestProfileCrudBroadcasts drives the app-managed profile CRUD wire: put, set
// default, and delete each mutate the store and broadcast the updated catalogue.
func TestProfileCrudBroadcasts(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	reg, err := session.NewProfileRegistry(session.ExecProfile{Name: "host", Target: session.TargetHost, Default: true})
	if err != nil {
		t.Fatal(err)
	}
	gw.driver.Profiles = reg

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "client_id": "pc"})
	readUntil(t, ws, "hello_ok")
	readUntil(t, ws, "profiles") // initial push on connect

	// Add a profile → broadcast with two entries.
	send(t, ws, map[string]any{"type": "profile_put", "profile_def": map[string]any{"name": "sandbox", "target": "sandbox"}})
	msg := readUntil(t, ws, "profiles")
	if items := msg["profiles"].([]any); len(items) != 2 {
		t.Fatalf("after put: %d profiles, want 2", len(items))
	}

	// Move the default marker to the new profile.
	send(t, ws, map[string]any{"type": "profile_set_default", "name": "sandbox"})
	if msg = readUntil(t, ws, "profiles"); msg["default"] != "sandbox" {
		t.Fatalf("default after set = %#v, want sandbox", msg["default"])
	}

	// Delete it → falls back to the remaining profile as default.
	send(t, ws, map[string]any{"type": "profile_delete", "name": "sandbox"})
	msg = readUntil(t, ws, "profiles")
	if items := msg["profiles"].([]any); len(items) != 1 {
		t.Fatalf("after delete: %d profiles, want 1", len(items))
	}
	if msg["default"] != "host" {
		t.Fatalf("default after delete = %#v, want host", msg["default"])
	}

	// A nameless profile is rejected with bad_profile.
	send(t, ws, map[string]any{"type": "profile_put", "profile_def": map[string]any{"name": ""}})
	if e := readUntil(t, ws, "error"); e["code"] != "bad_profile" {
		t.Fatalf("error code = %#v, want bad_profile", e["code"])
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
	ts, _, gw := newTestServerGW(t, nil)
	ws := dial(t, ts)
	// Present the matching hosts digest so the connect-time fast path suppresses the
	// proactive host_list push — this test drives the explicit request/broadcast path.
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "hosts_digest": hostsDigest(gw.hosts.List())})
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
	ts, _, gw := newTestServerGW(t, nil)
	ws := dial(t, ts)
	// Present the matching identities digest so the connect-time fast path suppresses
	// the proactive identity_list push — this test drives the explicit request path.
	send(t, ws, map[string]any{"type": "hello", "token": "secret", "identities_digest": identitiesDigest(gw.ids.List())})
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

	// "hey buddy, spawn a new session" -> asks for the full path.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy, spawn a new session"})
	d := readUntil(t, ws, "dialog")
	if d["state"] != "await_path" {
		t.Fatalf("expected await_path, got %v", d["state"])
	}

	// Speak the full path to an existing folder -> it resolves and moves straight
	// to the attach question (no sandbox configured, so no target step).
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "myproj")})
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

// TestClearThenDictateWithStaleID reproduces the "that session is gone" bug: a
// `clear` rotates the attached session's session_id and retires the old one, but an
// old app may keep routing utterances by the pre-rotation id even though the
// context_reset carries the new one. selectClientSession must recognize that retired
// id as the session it's already attached to and stay on it, instead of failing with
// "that session is gone."
func TestClearThenDictateWithStaleID(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Spawn + attach, capturing the session_id the app routes by.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "proj")})
	readUntil(t, ws, "dialog")
	send(t, ws, map[string]any{"type": "utterance", "text": "yes"})
	oldID, _ := readUntil(t, ws, "attached")["session_id"].(string)
	if oldID == "" {
		t.Fatal("expected a session_id on attach")
	}

	// Run a turn so the session is Started (clear refuses on an unstarted session).
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong", "session_id": oldID})
	if out := readUntil(t, ws, "output"); out["text"] != "pong" {
		t.Fatalf("expected pong, got %v", out["text"])
	}

	// Clear rotates the session_id and retires oldID; the context_reset carries the
	// fresh id so the app can re-key and refresh that session's rows.
	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy clear context", "session_id": oldID})
	reset := readUntil(t, ws, "context_reset")
	if newID, _ := reset["session_id"].(string); newID == "" || newID == oldID {
		t.Fatalf("context_reset should carry the rotated session_id (got %q, old %q)", reset["session_id"], oldID)
	}

	// The app dictates again, still keyed to the retired oldID. Before the fix this
	// tripped "that session is gone." (no output); now it must reach the session.
	send(t, ws, map[string]any{"type": "utterance", "text": "say pong", "session_id": oldID})
	if out := readUntil(t, ws, "output"); out["text"] != "pong" {
		t.Fatalf("stale-id dictation after clear should still reach the session, got %v", out["text"])
	}
}

// TestSwapAfterPrevSessionCleared reproduces the "the previous session is gone"
// swipe bug: connection C1 leaves session A (recording A's id as its swap target),
// then a second connection clears A — rotating A's session_id and retiring the old
// one. C1's prevSessionID now holds a retired id, so a plain byID lookup misses the
// very-much-alive session. A swap (right-to-left swipe) must resolve A by its prior
// id and land on it, instead of failing with "the previous session is gone."
func TestSwapAfterPrevSessionCleared(t *testing.T) {
	ts, root := newTestServer(t, nil)
	for _, d := range []string{"projA", "projB"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	c1 := dial(t, ts)
	send(t, c1, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, c1, "hello_ok")

	// C1 spawns A and runs a turn (so a later clear is allowed), then spawns B.
	send(t, c1, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, c1, "dialog")
	send(t, c1, map[string]any{"type": "utterance", "text": filepath.Join(root, "projA")})
	readUntil(t, c1, "dialog")
	send(t, c1, map[string]any{"type": "utterance", "text": "yes"})
	attachedA := readUntil(t, c1, "attached")
	nameA, _ := attachedA["name"].(string)
	idA, _ := attachedA["session_id"].(string)
	send(t, c1, map[string]any{"type": "utterance", "text": "say pong"})
	if out := readUntil(t, c1, "output"); out["text"] != "pong" {
		t.Fatalf("expected pong, got %v", out["text"])
	}
	send(t, c1, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, c1, "dialog")
	send(t, c1, map[string]any{"type": "utterance", "text": filepath.Join(root, "projB")})
	readUntil(t, c1, "dialog")
	send(t, c1, map[string]any{"type": "utterance", "text": "yes"})
	idB, _ := readUntil(t, c1, "attached")["session_id"].(string)

	// Bounce A -> B via session_id-routed selects so the swap target lands on A:
	// selecting a session records the one we were on as the previous.
	send(t, c1, map[string]any{"type": "utterance", "text": "hey buddy status", "session_id": idA})
	readUntil(t, c1, "attached")
	send(t, c1, map[string]any{"type": "utterance", "text": "hey buddy status", "session_id": idB})
	readUntil(t, c1, "attached") // now attached B, prevSessionID = A's id

	// A second connection clears A (routed by its session_id), rotating A's id and
	// retiring the id C1 still holds as its swap target.
	c2 := dial(t, ts)
	send(t, c2, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, c2, "hello_ok")
	send(t, c2, map[string]any{"type": "utterance", "text": "hey buddy clear context", "session_id": idA})
	readUntil(t, c2, "context_reset")

	// C1 swipes back to A. C1's prevSessionID is now A's retired id; before the fix
	// this reported "the previous session is gone." Now it must attach A.
	send(t, c1, map[string]any{"type": "utterance", "text": "hey buddy swap"})
	if a := readUntil(t, c1, "attached"); a["name"] != nameA {
		t.Fatalf("swap should land back on %q after prev was cleared, got %v", nameA, a["name"])
	}
}

// TestSpawnFuzzyPathAutoCorrects: a spoken path whose last segment only
// fuzzy-matches an existing folder ("mail" -> "mail_play") auto-corrects with no
// confirmation step and resolves straight to the attach question.
func TestSpawnFuzzyPathAutoCorrects(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "mail_play"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_path

	// The final segment "mail" fuzzy-matches "mail_play" -> auto-corrects and goes
	// straight to attach (no confirm step in the new flow).
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "mail")})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_attach" {
		t.Fatalf("expected await_attach after auto-correct, got %v", d["state"])
	}
}

// TestSpawnExactMatchNoConfirm: an exact path resolves straight to the attach
// question.
func TestSpawnExactMatchNoConfirm(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "mail_play"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_path
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "mail_play")})
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
	name := spawnAttachVoice(t, a, filepath.Join(root, "myproj"))

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

func TestUtteranceSessionIDSelectsDictationTarget(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	for _, name := range []string{"one", "two"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	firstRec := &session.Session{Name: "one", Dir: filepath.Join(root, "one"), SessionID: "sid-one", Target: session.TargetHost, Host: session.LocalHost}
	secondRec := &session.Session{Name: "two", Dir: filepath.Join(root, "two"), SessionID: "sid-two", Target: session.TargetHost, Host: session.LocalHost}
	if err := gw.store.Put(firstRec); err != nil {
		t.Fatal(err)
	}
	if err := gw.store.Put(secondRec); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(t.TempDir(), "prompts.txt")
	gw.driver.HostBin(fakeClaudeCapture(t, "ok", capPath))

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "attach", "name": firstRec.Name})
	if attached := readUntil(t, ws, "attached"); attached["session_id"] != firstRec.SessionID {
		t.Fatalf("setup attach session_id = %#v, want %q", attached["session_id"], firstRec.SessionID)
	}
	send(t, ws, map[string]any{"type": "utterance", "text": "targeted turn", "session_id": secondRec.SessionID})
	readUntil(t, ws, "output")

	if got := gw.store.Get(firstRec.Name).Started; got {
		t.Fatalf("first session Started = true; targeted utterance should not run there")
	}
	if got := gw.store.Get(secondRec.Name).Started; !got {
		t.Fatalf("second session Started = false; targeted utterance should run there")
	}
	data, err := os.ReadFile(capPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "targeted turn") {
		t.Fatalf("fake claude did not receive targeted prompt; capture:\n%s", data)
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

// TestSpawnUnresolvedPathReprompts: in session mode a spoken path whose final
// segment matches no real folder can't be placed, so the dialog reprompts and
// stays in await_path rather than offering to create it.
func TestSpawnUnresolvedPathReprompts(t *testing.T) {
	ts, root := newTestServer(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a new session"})
	readUntil(t, ws, "dialog") // await_path

	// A final segment that matches nothing -> reprompt, staying in await_path.
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "brandnew")})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_path" {
		t.Fatalf("expected await_path reprompt, got %v", d["state"])
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
	gw := New(cfg, store, hosts, ids, nil, driver, tmux.NewManager(), nil, nil)
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
	readUntil(t, ws, "dialog") // await_path
	send(t, ws, map[string]any{"type": "utterance", "text": filepath.Join(root, "myproj")})

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

// TestSpawnNamedNoLocationTakesFastPath: a spawn command with no spoken path can't
// take a fast path (there are no roots / home default anymore), so it drops into
// the interactive dialog and asks for the full path.
func TestSpawnNamedNoLocationTakesFastPath(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a session"})
	if d := readUntil(t, ws, "dialog"); d["state"] != "await_path" {
		t.Fatalf("expected await_path for a no-location spawn, got %v", d["state"])
	}
}

// TestSpawnWithPathTakesFastPath: "spawn a session in <full path>" (no "new") with
// a path that resolves cleanly spawns+attaches immediately, with no dialog.
func TestSpawnWithPathTakesFastPath(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	dir := filepath.Join(root, "myproj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "utterance", "text": "hey buddy spawn a session in " + dir})
	a := readUntil(t, ws, "attached")
	name, _ := a["name"].(string)
	rec := gw.store.Get(name)
	if rec == nil {
		t.Fatalf("session %q not persisted", name)
	}
	if rec.Dir != dir {
		t.Errorf("Dir = %q, want %q", rec.Dir, dir)
	}
	if rec.Target != session.TargetHost {
		t.Errorf("Target = %q, want %q (fast path defaults to host)", rec.Target, session.TargetHost)
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
	if d["state"] != "await_path" {
		t.Fatalf("expected await_path from transcribed utterance, got %v", d["state"])
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

// TestAdoptDistinctIDMakesNewSession verifies that adopting a session_id not yet
// registered always brings in that distinct session, even when the folder already
// hosts another one under a different id — a session_id is the sole identity, and
// a shared directory no longer forces a reuse.
func TestAdoptDistinctIDMakesNewSession(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	dir := t.TempDir()
	// A session that already lives in the folder under a different id.
	if err := gw.store.Put(&session.Session{Name: "proj", Dir: dir, SessionID: "id-live", Host: session.LocalHost}); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Adopt a DIFFERENT id for the same dir → a new distinct session, not a reuse.
	send(t, ws, map[string]any{"type": "adopt", "session_id": "id-other", "path": dir})
	a := readUntil(t, ws, "attached")
	if name, _ := a["name"].(string); name == "proj" {
		t.Fatalf("adopt reused the dir's existing session; want a distinct new one")
	}
	adopted := gw.store.GetBySessionID("id-other")
	if adopted == nil {
		t.Fatal("adopted session_id was not registered")
	}
	if gw.store.Get("proj") == nil {
		t.Fatal("the pre-existing dir-mate session was lost")
	}
}

// TestSpawnAtMakesNewSession verifies a directory is just the initial working
// dir, not the session's identity: spawning into a folder that already has a
// session mints a NEW distinct session ("proj-2") rather than re-attaching to the
// old one.
func TestSpawnAtMakesNewSession(t *testing.T) {
	ts, root := newTestServer(t, nil)
	dir := filepath.Join(root, "proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	// Spawn into the folder twice; the second spawn must be a fresh session with a
	// deduped name, and both must remain registered.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir})
	first := readUntil(t, ws, "attached")["name"].(string)
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir})
	second := readUntil(t, ws, "attached")["name"].(string)

	if second == first {
		t.Fatalf("second spawn reused session %q (want a new one)", first)
	}
	// Both survive in the session list (spawn_at emits it) — neither was collapsed.
	names := map[string]bool{}
	for _, s := range readUntil(t, ws, "session_list")["sessions"].([]any) {
		names[s.(map[string]any)["name"].(string)] = true
	}
	if !names[first] || !names[second] {
		t.Fatalf("both sessions should remain, got %v", names)
	}
}

// TestSpawnAtAlwaysMakesNewSession: every spawn is a fresh session regardless of
// directory or host — a folder is only the initial working dir, never an identity
// the spawn re-attaches to.
func TestSpawnAtAlwaysMakesNewSession(t *testing.T) {
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

	// Same folder, explicit remote host → a brand-new session.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "host_name": "remote"})
	remote := readUntil(t, ws, "attached")["name"].(string)
	if remote == local {
		t.Fatalf("remote-host spawn reused the localhost session %q", local)
	}

	// Re-picking the same host still mints a distinct third session.
	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "host_name": "remote"})
	third := readUntil(t, ws, "attached")["name"].(string)
	if third == remote || third == local {
		t.Fatalf("re-spawn reused an existing session %q (want a new one)", third)
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

// TestSpawnAtPersistsProfile verifies the picker can choose an execution
// profile at spawn time and that registered-session messages carry it back.
func TestSpawnAtPersistsProfile(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	reg, err := session.NewProfileRegistry(
		session.ExecProfile{Name: "host", Target: session.TargetHost, Default: true},
		session.ExecProfile{Name: "open", Target: session.TargetHost, Env: map[string]string{"OLLAMA_BASE_URL": "http://ollama:11434"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	gw.driver.Profiles = reg
	dir := filepath.Join(root, "profiled")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "spawn_at", "path": dir, "profile": "open"})
	attached := readUntil(t, ws, "attached")
	if attached["profile"] != "open" {
		t.Fatalf("attached profile = %#v, want open", attached["profile"])
	}
	rec := gw.store.GetByDirHost(dir, session.LocalHost)
	if rec == nil || rec.Profile != "open" {
		t.Fatalf("stored profile = %#v, want open", rec)
	}

	list := readUntil(t, ws, "session_list")
	row := list["sessions"].([]any)[0].(map[string]any)
	if row["profile"] != "open" {
		t.Fatalf("session_list profile = %#v, want open", row["profile"])
	}

	send(t, ws, map[string]any{"type": "discover"})
	discovered := readUntil(t, ws, "discovered")
	for _, item := range discovered["sessions"].([]any) {
		row := item.(map[string]any)
		if row["dir"] == dir {
			if row["profile"] != "open" {
				t.Fatalf("discovered profile = %#v, want open", row["profile"])
			}
			return
		}
	}
	t.Fatalf("spawned session missing from discovered: %v", discovered["sessions"])
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
	spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))

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

func TestClearPublishesFreshSessionIDForTargetedTurns(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)
	if err := os.MkdirAll(filepath.Join(root, "myproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	capPath := filepath.Join(t.TempDir(), "prompts.txt")
	gw.driver.HostBin(fakeClaudeCapture(t, "ok", capPath))

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	name := spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))
	send(t, ws, map[string]any{"type": "utterance", "text": "first turn"})
	readUntil(t, ws, "output")
	origID := gw.store.Get(name).SessionID

	send(t, ws, map[string]any{"type": "clear"})
	reset := readUntil(t, ws, "context_reset")
	newID, _ := reset["session_id"].(string)
	if newID == "" || newID == origID {
		t.Fatalf("clear context_reset session_id = %q, want fresh id distinct from %q", newID, origID)
	}
	if reset["name"] != name {
		t.Fatalf("clear context_reset name = %#v, want %q", reset["name"], name)
	}
	readUntil(t, ws, "say")

	send(t, ws, map[string]any{"type": "utterance", "text": "after clear", "session_id": newID})
	readUntil(t, ws, "output")
	if rec := gw.store.Get(name); rec == nil || !rec.Started {
		t.Fatal("targeted turn after clear did not start the rotated session")
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

	name := spawnAttachVoice(t, ws, filepath.Join(root, "myproj"))

	// First real turn establishes context.
	send(t, ws, map[string]any{"type": "utterance", "text": "first turn"})
	readUntil(t, ws, "output")
	origID := gw.store.Get(name).SessionID

	// Compress: a background summary turn, then a rotation. The context_reset carries
	// the rotated id; the confirming say lands after the summary completes.
	send(t, ws, map[string]any{"type": "compress"})
	reset := readUntil(t, ws, "context_reset")
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
	if reset["session_id"] != rec.SessionID {
		t.Fatalf("compress context_reset session_id = %#v, want %q", reset["session_id"], rec.SessionID)
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

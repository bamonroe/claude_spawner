package gateway

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

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

package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

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

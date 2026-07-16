package gateway

import (
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/gorilla/websocket"
)

// TestCatalogueDigestFold exercises the order-independence and change-sensitivity
// the skip-if-equal fast path relies on: an empty catalogue folds to all-zero,
// record order doesn't matter, and any add / payload edit / timestamp-only edit /
// delete flips the digest.
func TestCatalogueDigestFold(t *testing.T) {
	empty := hostsDigest(nil)
	if empty != "0000000000000000" {
		t.Fatalf("empty hosts digest = %q, want all-zero", empty)
	}

	a := &session.Host{Name: "a", Address: "1.2.3.4", UpdatedAt: 100}
	b := &session.Host{Name: "b", Address: "5.6.7.8", UpdatedAt: 200}
	base := hostsDigest([]*session.Host{a, b})
	if base == empty {
		t.Fatal("a non-empty catalogue must not fold to the empty digest")
	}
	if rev := hostsDigest([]*session.Host{b, a}); rev != base {
		t.Fatalf("digest is order-dependent: %q vs %q", base, rev)
	}

	cases := map[string][]*session.Host{
		"add":            {a, b, {Name: "c", UpdatedAt: 1}},
		"delete":         {a},
		"payload edit":   {{Name: "a", Address: "9.9.9.9", UpdatedAt: 100}, b},
		"timestamp only": {{Name: "a", Address: "1.2.3.4", UpdatedAt: 101}, b},
	}
	for name, hosts := range cases {
		if d := hostsDigest(hosts); d == base {
			t.Errorf("%s did not flip the digest (still %q)", name, d)
		}
	}
}

// TestSettingsDigestFold covers the fifth catalogue's digest: empty folds to
// all-zero, order doesn't matter, and any add / value edit / timestamp edit flips
// it — plus a KNOWN HEX pinned byte-for-byte against the Kotlin
// CatalogueDigest.settings fold (see CatalogueDigestTest.kt), which guarantees the
// two languages compute the identical value for the skip-if-equal fast path.
func TestSettingsDigestFold(t *testing.T) {
	empty := settingsDigest(mustSettings(t))
	if empty != "0000000000000000" {
		t.Fatalf("empty settings digest = %q, want all-zero", empty)
	}

	// Known-hex fixture, mirrored exactly in the Kotlin test.
	pinned := mustSettings(t,
		session.SettingRecord{Key: "summary_only", Value: "true", UpdatedAt: 100},
		session.SettingRecord{Key: "auto_compress", Value: "false", UpdatedAt: 200},
	)
	const wantHex = "d7a850f0b07c87bd"
	if d := settingsDigest(pinned); d != wantHex {
		t.Fatalf("settings digest = %q, want pinned %q (Kotlin parity)", d, wantHex)
	}
	// Order-independence: inserting the same records in the other order matches.
	rev := mustSettings(t,
		session.SettingRecord{Key: "auto_compress", Value: "false", UpdatedAt: 200},
		session.SettingRecord{Key: "summary_only", Value: "true", UpdatedAt: 100},
	)
	if d := settingsDigest(rev); d != wantHex {
		t.Fatalf("settings digest is order-dependent: %q vs pinned %q", d, wantHex)
	}

	base := settingsDigest(pinned)
	for name, recs := range map[string][]session.SettingRecord{
		"add":            {{Key: "summary_only", Value: "true", UpdatedAt: 100}, {Key: "auto_compress", Value: "false", UpdatedAt: 200}, {Key: "warm_compress", Value: "true", UpdatedAt: 1}},
		"value edit":     {{Key: "summary_only", Value: "false", UpdatedAt: 100}, {Key: "auto_compress", Value: "false", UpdatedAt: 200}},
		"timestamp only": {{Key: "summary_only", Value: "true", UpdatedAt: 101}, {Key: "auto_compress", Value: "false", UpdatedAt: 200}},
	} {
		if d := settingsDigest(mustSettings(t, recs...)); d == base {
			t.Errorf("%s did not flip the settings digest (still %q)", name, d)
		}
	}
}

// mustSettings builds an in-memory SettingKV seeded with the given records.
func mustSettings(t *testing.T, recs ...session.SettingRecord) *session.SettingKV {
	t.Helper()
	s, err := session.OpenSettingKV("")
	if err != nil {
		t.Fatal(err)
	}
	for i := range recs {
		r := recs[i]
		if err := s.Put(&r); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// stallModelRefresh disables the throttled background model re-discovery for this
// server so it can't asynchronously re-broadcast `agents` during a test's connect
// window (which would race the connect-time suppression assertions).
func stallModelRefresh(gw *Server) {
	gw.modelMu.Lock()
	gw.modelRefreshed = time.Now()
	gw.modelMu.Unlock()
}

// connectCatalogueTypes returns the set of message types the server sent between
// hello_ok and the pong for a trailing ping — i.e. exactly its connect-time
// catalogue broadcasts. The ping/pong is a sentinel: it's dispatched only after
// authenticate() has already queued every connect-time send on the ordered socket.
func connectCatalogueTypes(t *testing.T, ws *websocket.Conn) map[string]bool {
	t.Helper()
	send(t, ws, map[string]any{"type": "ping"})
	seen := map[string]bool{}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			t.Fatalf("reading connect messages: %v", err)
		}
		if m["type"] == "pong" {
			return seen
		}
		if typ, _ := m["type"].(string); typ != "" {
			seen[typ] = true
		}
	}
}

// seedCatalogues gives the server a non-empty entry in all four app-managed
// catalogues and returns the server's digest of each.
func seedCatalogues(t *testing.T, gw *Server) (hostsD, idsD, profD, provD string) {
	t.Helper()
	reg, err := session.NewProfileRegistry(session.ExecProfile{Name: "host", Target: session.TargetHost, Default: true})
	if err != nil {
		t.Fatal(err)
	}
	gw.driver.Profiles = reg
	if err := gw.hosts.Put(&session.Host{Name: "box", Address: "10.0.0.1", UpdatedAt: 5}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.ids.Create("id1", "bam", "", true, 5); err != nil {
		t.Fatal(err)
	}
	return hostsDigest(gw.hosts.List()),
		identitiesDigest(gw.ids.List()),
		profilesDigest(gw.driver.ProfileRegistry().List()),
		providersDigest(gw.driver.Registry(), gw.driver.ProviderSettings())
}

// TestConnectDigestSuppressesUnchangedCatalogues: a hello whose four digests match
// the server's suppresses all four connect-time catalogue broadcasts.
func TestConnectDigestSuppressesUnchangedCatalogues(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	stallModelRefresh(gw)
	hd, idd, pd, prd := seedCatalogues(t, gw)

	ws := dial(t, ts)
	send(t, ws, map[string]any{
		"type": "hello", "token": "secret",
		"hosts_digest": hd, "identities_digest": idd,
		"profiles_digest": pd, "providers_digest": prd,
	})
	readUntil(t, ws, "hello_ok")

	seen := connectCatalogueTypes(t, ws)
	for _, typ := range []string{"host_list", "identity_list", "profiles", "agents"} {
		if seen[typ] {
			t.Errorf("catalogue %q re-sent on connect despite a matching digest", typ)
		}
	}
}

// TestConnectDigestBroadcastsChangedCatalogues: a hello with stale (mismatching)
// digests re-broadcasts all four catalogues, as an older/first-time client does.
func TestConnectDigestBroadcastsChangedCatalogues(t *testing.T) {
	ts, _, gw := newTestServerGW(t, nil)
	stallModelRefresh(gw)
	seedCatalogues(t, gw)

	ws := dial(t, ts)
	send(t, ws, map[string]any{
		"type": "hello", "token": "secret",
		"hosts_digest": "stale", "identities_digest": "stale",
		"profiles_digest": "stale", "providers_digest": "stale",
	})
	readUntil(t, ws, "hello_ok")

	seen := connectCatalogueTypes(t, ws)
	for _, typ := range []string{"host_list", "identity_list", "profiles", "agents"} {
		if !seen[typ] {
			t.Errorf("catalogue %q not broadcast on connect despite a stale digest", typ)
		}
	}
}

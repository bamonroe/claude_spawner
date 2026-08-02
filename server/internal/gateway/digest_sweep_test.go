package gateway

import (
	"testing"
)

// Regression coverage for moving the sweep off the inbound loop: a request sent
// straight after `digest` is still answered, and the connection is not left
// wedged by the handoff. This does NOT prove non-blocking — the in-memory test
// harness reads its transcripts instantly, so an inline sweep would pass too.
// The property it guards is that the async path is wired up and doesn't drop or
// deadlock the following request; the latency win is measured against the real
// server, not here.
func TestDigestSweepDoesNotBlockTheInboundLoop(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "digest"})
	// Sent immediately behind the sweep: if the sweep held the loop, this would
	// not be answered until it finished.
	send(t, ws, map[string]any{"type": "list_sessions"})
	readUntil(t, ws, "session_list")
}

// A sweep still reports its items, a duplicate request doesn't produce a hang or
// a malformed reply, and the connection stays usable afterwards.
func TestDigestSweepCoalescesAndReports(t *testing.T) {
	ts, _ := newTestServer(t, nil)
	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")

	send(t, ws, map[string]any{"type": "digest"})
	send(t, ws, map[string]any{"type": "digest"})
	if m := readUntil(t, ws, "digests"); m["items"] == nil {
		t.Fatal("digests message carried no items field")
	}
	// The connection stays usable afterwards.
	send(t, ws, map[string]any{"type": "list_sessions"})
	readUntil(t, ws, "session_list")
}

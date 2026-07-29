package gateway

import (
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// A direct say/error sent while attached must be attributed to the attached
// session, so a device viewing a different session files the notice under the
// right chat log instead of whatever it currently shows. Hub-fanned frames
// (already stamped) and non-notice frames are left untouched, and a detached
// connection leaves the notice unstamped for the client's general bucket.
func TestStampSessionAttributesDirectNotices(t *testing.T) {
	attached := &conn{}
	attached.setAttached(&session.Session{SessionID: "sid-A"})
	detached := &conn{}

	sidOf := func(m map[string]any) string { s, _ := m["session_id"].(string); return s }

	t.Run("say stamped with attached id", func(t *testing.T) {
		m := msgSay("cleared. starting fresh.")
		attached.stampSession(m)
		if got := sidOf(m); got != "sid-A" {
			t.Fatalf("say session_id = %q, want %q", got, "sid-A")
		}
	})

	t.Run("error stamped with attached id", func(t *testing.T) {
		m := msgError("transcribe_failed", "boom")
		attached.stampSession(m)
		if got := sidOf(m); got != "sid-A" {
			t.Fatalf("error session_id = %q, want %q", got, "sid-A")
		}
	})

	t.Run("existing id not overwritten (hub-fanned)", func(t *testing.T) {
		m := msgSay("compressed.")
		m["session_id"] = "sid-B"
		attached.stampSession(m)
		if got := sidOf(m); got != "sid-B" {
			t.Fatalf("session_id = %q, want the pre-set %q", got, "sid-B")
		}
	})

	t.Run("non-notice frame not stamped", func(t *testing.T) {
		m := map[string]any{"type": "whisper_model", "model": "medium.en"}
		attached.stampSession(m)
		if _, present := m["session_id"]; present {
			t.Fatalf("non-notice frame gained a session_id: %v", m)
		}
	})

	t.Run("detached leaves notice unstamped", func(t *testing.T) {
		m := msgSay("picking up where we left off.")
		detached.stampSession(m)
		if _, present := m["session_id"]; present {
			t.Fatalf("detached say gained a session_id: %v", m)
		}
	})
}

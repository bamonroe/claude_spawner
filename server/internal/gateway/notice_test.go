package gateway

import (
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// readNoticeSoon reads until a `notice` arrives, or reports that none did within
// a short window. Unlike readUntil it does not fail the test on a timeout — the
// point of these tests is that one connection gets the frame and the other must
// not.
func readNoticeSoon(t *testing.T, ws *websocket.Conn) (map[string]any, bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	_ = ws.SetReadDeadline(deadline)
	for time.Now().Before(deadline) {
		var m map[string]any
		if err := ws.ReadJSON(&m); err != nil {
			return nil, false
		}
		if m["type"] == "notice" {
			return m, true
		}
	}
	return nil, false
}

// broadcastNotice targets exactly the connections that the session job's fanout
// misses: a device connected but attached elsewhere (or detached) gets the
// heads-up frame, while the device attached to that very session does not —
// it is already receiving the real turn output and a notice would double-report
// the finished job.
func TestBroadcastNoticeSkipsTheAttachedConnection(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)

	attached := dial(t, ts)
	send(t, attached, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, attached, "hello_ok")
	name := spawnSession(t, attached, root)

	rec := gw.store.Get(name)
	if rec == nil {
		t.Fatalf("session %q not in store", name)
	}

	// A second device: connected, but looking at another screen.
	elsewhere := dial(t, ts)
	send(t, elsewhere, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, elsewhere, "hello_ok")

	gw.broadcastNotice(rec, "the background job finished")

	m, ok := readNoticeSoon(t, elsewhere)
	if !ok {
		t.Fatal("the connected-but-unattached device should have received the notice")
	}
	if got := m["name"]; got != name {
		t.Errorf("notice name = %v, want %q", got, name)
	}
	if got := m["session_id"]; got != rec.SessionID {
		t.Errorf("notice session_id = %v, want %q", got, rec.SessionID)
	}
	if got, _ := m["text"].(string); got != "the background job finished" {
		t.Errorf("notice text = %q, want the summary", got)
	}

	if _, ok := readNoticeSoon(t, attached); ok {
		t.Error("the attached device must not get a notice — it already receives the turn output")
	}
}

// The notice is a heads-up rendered as a badge/toast, so a long reply is
// truncated to noticeMax with an ellipsis rather than shipped whole.
func TestBroadcastNoticeTruncatesLongText(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)

	owner := dial(t, ts)
	send(t, owner, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, owner, "hello_ok")
	name := spawnSession(t, owner, root)
	rec := gw.store.Get(name)

	elsewhere := dial(t, ts)
	send(t, elsewhere, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, elsewhere, "hello_ok")

	gw.broadcastNotice(rec, strings.Repeat("a", noticeMax+50))

	m, ok := readNoticeSoon(t, elsewhere)
	if !ok {
		t.Fatal("expected a notice")
	}
	text, _ := m["text"].(string)
	if !strings.HasSuffix(text, "…") {
		t.Errorf("a truncated notice should end in an ellipsis, got %q", text)
	}
	if n := len([]rune(text)); n > noticeMax+1 {
		t.Errorf("notice text is %d runes, want at most %d plus the ellipsis", n, noticeMax)
	}
}

// An empty summary (or a nil session) is not worth a frame: broadcastNotice
// drops it rather than flashing an empty badge on every connected device.
func TestBroadcastNoticeDropsEmpty(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)

	owner := dial(t, ts)
	send(t, owner, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, owner, "hello_ok")
	name := spawnSession(t, owner, root)
	rec := gw.store.Get(name)

	elsewhere := dial(t, ts)
	send(t, elsewhere, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, elsewhere, "hello_ok")

	gw.broadcastNotice(rec, "   ")
	gw.broadcastNotice(nil, "orphaned")

	if _, ok := readNoticeSoon(t, elsewhere); ok {
		t.Error("an empty summary should not produce a notice frame")
	}
}

package gateway

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeClaudeSlow is fakeClaude with a short sleep before the result, so a test
// has a deterministic window while the turn is in flight (j.running == true).
func fakeClaudeSlow(t *testing.T, reply string, d time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeclaude-slow.sh")
	ms := int(d.Milliseconds())
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init"}'` + "\n" +
		"sleep 0." + leftPad(ms) + "\n" +
		`echo '{"type":"result","subtype":"success","result":"` + reply + `","session_id":"fake"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func leftPad(ms int) string {
	s := itoa(ms)
	for len(s) < 3 {
		s = "0" + s
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func waitIdle(t *testing.T, j *sessionJob) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !j.isRunning() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("turn did not finish within 5s")
}

// recorder is a stand-in for an attached connection's sink. ok controls whether
// its writes "succeed" (a healthy socket) or "fail" (a briefly-unreachable phone,
// as c.send reports after the write deadline).
type recorder struct {
	mu   sync.Mutex
	ok   bool
	msgs []map[string]any
}

func (r *recorder) sink(v any) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := v.(map[string]any); ok {
		r.msgs = append(r.msgs, m)
	}
	return r.ok
}

func (r *recorder) gotOutput(text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if m["type"] == "output" && m["text"] == text {
			return true
		}
	}
	return false
}

// TestBufferedReplyRedeliveredAfterFailedSend is the regression test for the
// "follow-up reply never showed in the app" bug.
//
// Scenario: the phone is attached but its socket is momentarily unreachable
// (backgrounded / a mobile stall) exactly when a turn's reply is broadcast — the
// slow local model widens this window but it is not model-specific. The write
// fails, so the reply is buffered UNDELIVERED. Before the fix, the next turn's
// startTurn wiped that buffer and the reply was lost for good. After the fix, the
// buffered reply is redelivered via flushPending the moment the socket accepts a
// write again (at the next turn's start), so it still reaches the app.
func TestBufferedReplyRedeliveredAfterFailedSend(t *testing.T) {
	ts, root, gw := newTestServerGW(t, nil)

	ws := dial(t, ts)
	send(t, ws, map[string]any{"type": "hello", "token": "secret"})
	readUntil(t, ws, "hello_ok")
	name := spawnSession(t, ws, root)

	sess := gw.store.Get(name)
	if sess == nil {
		t.Fatalf("session %q not in store", name)
	}
	j := gw.jobFor(sess.SessionID)

	// The phone is attached but its socket is stalled: the sole sink records what
	// the server tried to send and reports a FAILED write.
	phone := &recorder{ok: false}
	j.mu.Lock()
	j.sinks = map[*conn]func(any) bool{{}: phone.sink}
	j.mu.Unlock()

	// Turn one — the answer the user asks for while the socket is stalled.
	gw.driver.HostBin(fakeClaudeSlow(t, "the-missed-answer", 60*time.Millisecond))
	if !gw.startTurn(sess, "what is the population", false, false) {
		t.Fatal("startTurn (one) refused")
	}
	waitIdle(t, j)

	// The write failed, so the reply is buffered and undelivered.
	j.mu.Lock()
	buffered := j.final != nil
	delivered := j.delivered
	j.mu.Unlock()
	if !buffered || delivered {
		t.Fatalf("after failed send want buffered && !delivered, got buffered=%v delivered=%v", buffered, delivered)
	}

	// The phone's socket recovers. Model this by swapping in a healthy sink.
	phone.ok = true
	j.mu.Lock()
	j.sinks = map[*conn]func(any) bool{{}: phone.sink}
	j.mu.Unlock()

	// Turn two — the user asks the next question. Its startTurn no longer discards
	// the buffered reply; flushPending redelivers it before the new turn runs.
	gw.driver.HostBin(fakeClaudeSlow(t, "the-second-answer", 60*time.Millisecond))
	if !gw.startTurn(sess, "what time is it", false, false) {
		t.Fatal("startTurn (two) refused")
	}
	waitIdle(t, j)

	// The previously-missed reply finally reached the app...
	if !phone.gotOutput("the-missed-answer") {
		t.Fatal("the buffered reply from turn one was never redelivered — it was lost")
	}
	// ...and so did the new turn's reply.
	if !phone.gotOutput("the-second-answer") {
		t.Fatal("turn two's reply was not delivered")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.final != nil || !j.delivered {
		t.Fatalf("after both delivered want final==nil && delivered, got final=%v delivered=%v", j.final, j.delivered)
	}
}

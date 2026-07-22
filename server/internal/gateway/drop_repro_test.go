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
	// the server tried to send and reports a FAILED write. The connection pointer is
	// STABLE across the stall and recovery — the same socket recovers in place, which
	// is why the per-connection buffer keys on it.
	phone := &recorder{ok: false}
	pc := &conn{}
	j.mu.Lock()
	j.sinks = map[*conn]func(any) bool{pc: phone.sink}
	j.mu.Unlock()

	// Turn one — the answer the user asks for while the socket is stalled.
	gw.driver.HostBin(fakeClaudeSlow(t, "the-missed-answer", 60*time.Millisecond))
	if !gw.startTurn(sess, "what is the population", false, false) {
		t.Fatal("startTurn (one) refused")
	}
	waitIdle(t, j)

	// The write failed, so the reply is buffered undelivered against this connection.
	j.mu.Lock()
	buffered := j.pending[pc] != nil
	j.mu.Unlock()
	if !buffered {
		t.Fatalf("after failed send want a per-connection pending buffer, got none (pending=%v)", j.pending)
	}

	// The phone's socket recovers in place (same connection, healthy writes now).
	phone.ok = true

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
	if j.pending[pc] != nil {
		t.Fatalf("after redelivery the connection's pending buffer should be cleared, got %v", j.pending[pc])
	}
}

// TestMidTurnAttachReplaysStreamedOutput is the regression test for the "webapp
// lost the middle of a long turn" bug. A connection that attaches or reconnects
// while a turn is in flight has none of the prose already streamed, and those
// in-flight steps aren't in the on-disk transcript yet, so a history refetch can't
// backfill them — bindJob used to only send a "still working" nudge. Now the job
// buffers the turn's streamed `output` frames and replayInFlight catches a fresh
// sink up on all of them.
func TestMidTurnAttachReplaysStreamedOutput(t *testing.T) {
	j := &sessionJob{}
	j.mu.Lock()
	j.beginTurn(func() {})
	j.mu.Unlock()

	// The turn streams two prose steps plus an ephemeral activity breadcrumb; only
	// the output prose should be buffered for replay.
	j.emit(msgOutput("cpt", "step one", "t1", true, nil))
	j.emit(msgActivity("🤔 thinking…"))
	j.emit(msgOutput("cpt", "step two", "t1", true, nil))

	j.mu.Lock()
	if got := len(j.turnFrames); got != 2 {
		t.Fatalf("want 2 buffered output frames (activity excluded), got %d", got)
	}
	// A device attaches mid-turn: replayInFlight must hand it every streamed step.
	late := &recorder{ok: true}
	j.replayInFlight(late.sink)
	j.mu.Unlock()

	if !late.gotOutput("step one") || !late.gotOutput("step two") {
		t.Fatal("mid-turn attach was not caught up on the turn's streamed output")
	}

	// The turn ends; a NEW turn resets the buffer so last turn's prose isn't replayed
	// to someone attaching later (they get history + any buffered terminal reply).
	j.finish(msgOutput("cpt", "final", "t1", false, nil))
	j.mu.Lock()
	j.beginTurn(func() {})
	stale := j.turnFrames
	j.mu.Unlock()
	if stale != nil {
		t.Fatalf("beginTurn must clear the replay buffer, got %v", stale)
	}
	// Output emitted when no turn is running is not buffered.
	j.finish(msgOutput("cpt", "x", "t2", false, nil)) // running=false
	j.emit(msgOutput("cpt", "after", "t2", true, nil))
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.turnFrames) != 0 {
		t.Fatalf("output with no turn running must not be buffered, got %v", j.turnFrames)
	}
}

// TestBufferedReplyIsPerConnection is the regression test for the multi-client
// drop: two devices attached to one session, and the turn's reply reaches device A
// but the write to device B fails (B briefly unreachable). The OLD single-buffer
// design treated "reached at least one client" as delivered and dropped the buffer,
// stranding B. The per-connection buffer must instead redeliver to B alone while
// leaving A untouched.
func TestBufferedReplyIsPerConnection(t *testing.T) {
	live := &conn{}
	stalled := &conn{}
	a := &recorder{ok: true}  // device A: healthy socket
	b := &recorder{ok: false} // device B: write fails
	j := &sessionJob{running: true, sinks: map[*conn]func(any) bool{
		live:    a.sink,
		stalled: b.sink,
	}}

	j.finish(map[string]any{"type": "output", "text": "the-answer"})

	if !a.gotOutput("the-answer") {
		t.Fatal("device A should have received the reply directly")
	}
	j.mu.Lock()
	strandedBuffered := j.pending[stalled] != nil
	aBuffered := j.pending[live] != nil
	orphan := j.orphan
	j.mu.Unlock()
	if !strandedBuffered {
		t.Fatal("device B's missed reply must be buffered against ITS connection, not dropped because A succeeded")
	}
	if aBuffered {
		t.Fatal("device A received the reply — it must NOT have a pending buffer")
	}
	if orphan != nil {
		t.Fatal("a live client (A) got the reply — no orphan copy should be kept")
	}

	// B's socket recovers; flushPending redelivers to B alone (A gets nothing extra).
	b.ok = true
	j.flushPending()
	if !b.gotOutput("the-answer") {
		t.Fatal("device B's buffered reply was never redelivered — it was lost")
	}
	if n := len(a.msgs); n != 1 {
		t.Fatalf("device A should have received exactly one message, got %d (redelivery leaked to A)", n)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.pending[stalled] != nil {
		t.Fatal("B's pending buffer should be cleared after redelivery")
	}
}

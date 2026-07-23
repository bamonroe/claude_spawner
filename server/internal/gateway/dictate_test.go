package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

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
// kept as the orphan buffer so it's delivered on reconnect — not treated as
// delivered and lost, which left the app stuck on "running the command".
func TestJobBuffersWhenSinkFails(t *testing.T) {
	dead := &conn{}
	j := &sessionJob{running: true, sinks: map[*conn]func(any) bool{
		dead: func(any) bool { return false }, // simulate a failed write
	}}
	j.finish(map[string]any{"type": "output", "text": "done"})
	if j.orphan == nil {
		t.Fatal("result must be kept as the orphan buffer when no live client got it")
	}

	// A fresh connection attaching (reconnect) then gets the orphan result; the
	// buffer is freed. This mirrors bindJob's orphan-delivery branch.
	var got any
	if sink := func(v any) bool { got = v; return true }; j.orphan != nil {
		if sink(j.orphan) {
			j.orphan = nil
		}
	}
	if got == nil || j.orphan != nil {
		t.Fatalf("orphan result not delivered on reconnect: got=%v orphan=%v", got, j.orphan)
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

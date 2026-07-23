package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// stampTurn tags a turn-terminal frame (ask / turn_stopped / error / the
// compress say) with the turn's id. Every frame that can be buffered and
// redelivered on reconnect must carry the id, or the client has no way to tell
// a buffered redelivery from a fresh event (an unstamped ask would re-present
// its questions — and re-speak them — on every reconnect).
func stampTurn(m map[string]any, turn string) map[string]any {
	m["turn"] = turn
	return m
}

// newTurnID mints the opaque per-turn id stamped on every `output` frame of one
// turn (chunks + close) — the client's dedup key, since text equality between a
// chunk and its close is not guaranteed and a close can be redelivered.
func newTurnID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Fall back to a time-based id; uniqueness per turn is all that matters.
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// diffSummary returns a compact `git diff --stat` of the working tree in dir, or
// "" if dir isn't a git repo, has no uncommitted changes, or on any error. Capped
// so a huge diff can't flood the app.
func diffSummary(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "diff", "--stat", "--stat-width=60").Output()
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > 12 {
		lines = append(lines[:11], "… (+"+strconv.Itoa(len(lines)-11)+" more)")
	}
	return strings.Join(lines, "\n")
}

// logField trims a prompt/reply to one tidy line for the turn log.
func logField(s string) string {
	const max = 500
	out := make([]rune, 0, max)
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' {
			r = ' '
		}
		out = append(out, r)
		if len(out) >= max {
			return string(out) + "…"
		}
	}
	return string(out)
}

// sessionJob is a per-session hub for a dictation turn, running independently of
// any WebSocket connection so a long Claude job survives the app disconnecting.
// It fans out live events (tool breadcrumbs, the final result) to EVERY currently
// attached connection's sink — so several devices watching the same session all
// see the turn live — and buffers the final result PER connection whose write
// failed (redelivered to just that connection), plus an orphan copy for the next
// attach when the turn finished with no live connection at all.
//
// The hub persists across turns (it holds the sink set + any buffered result)
// until the session is deleted. Sinks are maintained by bindJob/unbindJob as
// connections attach/detach, independent of which one dictated the turn.
//
// Lock ordering: emit/finish/bindJob call the sinks (conn.jobSink -> conn.send)
// while holding j.mu, and conn.send takes conn.wmu — so the order is ALWAYS
// j.mu -> conn.wmu, never the reverse. A sink must not call back into the job
// (it would re-enter j.mu and deadlock); it only does a websocket write.
type sessionJob struct {
	mu      sync.Mutex
	running bool
	// pending holds, PER attached connection, a turn's result whose write to THAT
	// connection failed (a briefly-unreachable phone) — so it is redelivered to
	// exactly the connection(s) that missed it, independent of whether a co-attached
	// device received the same turn. One client succeeding no longer strands another.
	pending map[*conn]map[string]any
	// orphan is the last turn's result when it reached NO live connection at all
	// (nobody attached, or every attached write failed) — handed to the next attach.
	orphan  map[string]any
	sinks   map[*conn]func(any) bool // every attached connection's sink
	cancel  context.CancelFunc       // aborts the running turn's claude child
	aborted bool                     // set when the current turn was cancelled on request
	// turnFrames holds the CURRENT in-flight turn's streamed `output` frames so a
	// connection that attaches or reconnects mid-turn can be replayed everything the
	// turn has produced so far — the in-flight steps aren't in the on-disk transcript
	// yet, so a history refetch can't backfill them. Reset at each turn's start,
	// capped at maxTurnFrames (oldest dropped; the tail is what a late joiner needs,
	// and the whole turn lands in history once it finishes).
	turnFrames []map[string]any
}

// maxTurnFrames bounds the per-turn replay buffer so a very long agentic turn
// can't grow it without limit; older streamed frames beyond this fall out of the
// live catch-up and are recovered from history once the turn completes.
const maxTurnFrames = 400

func (j *sessionJob) isRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// hasSink reports whether at least one connection is currently attached to the
// hub. The idle job-notify ticker checks this before driving an autonomous "your
// job finished" turn — with nobody listening there's no one to tell out loud, so
// the completion just stays in PendingNotes for the next dictation/attach.
func (j *sessionJob) hasSink() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.sinks) > 0
}

// broadcast sends msg to every attached sink, returning true if at least one
// reached a live client. Call with j.mu held.
func (j *sessionJob) broadcast(msg any) bool {
	reached := false
	for _, sink := range j.sinks {
		if sink(msg) {
			reached = true
		}
	}
	return reached
}

// broadcastExcept sends msg to every attached sink except `origin`'s.
func (j *sessionJob) broadcastExcept(origin *conn, msg any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for c, sink := range j.sinks {
		if c != origin {
			sink(msg)
		}
	}
}

// emit fans out a live-only event (e.g. a tool breadcrumb) to every attached
// connection; dropped for any that are gone / if nobody's attached.
func (j *sessionJob) emit(msg map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	// Record streamed reply prose for mid-turn attach/reconnect catch-up (bindJob
	// replays it). Only `output` frames carry chat content; ephemeral activity /
	// pending / rate-limit frames aren't worth replaying.
	if j.running && msg["type"] == "output" {
		j.turnFrames = append(j.turnFrames, msg)
		if len(j.turnFrames) > maxTurnFrames {
			j.turnFrames = j.turnFrames[len(j.turnFrames)-maxTurnFrames:]
		}
	}
	j.broadcast(msg)
}

// replayInFlight sends the current turn's streamed `output` frames to a single
// freshly-bound sink, catching a mid-turn attach/reconnect up on reply prose that
// isn't in the on-disk transcript yet (so a history refetch can't backfill it).
// Call with j.mu held.
func (j *sessionJob) replayInFlight(sink func(any) bool) {
	for _, f := range j.turnFrames {
		sink(f)
	}
}

func (j *sessionJob) finish(final map[string]any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.cancel = nil
	j.deliver(final)
}

// deliver hands `final` to every attached sink. Any connection whose write fails
// (backgrounded / a mobile stall past the write deadline) gets `final` recorded
// in its per-connection pending buffer so flushPending redelivers to exactly that
// connection later — a co-attached device receiving the turn does NOT clear it.
// If no attached connection was reachable at all, `final` is stashed as the orphan
// buffer for the next attach. Call with j.mu held.
func (j *sessionJob) deliver(final map[string]any) {
	reached := false
	for c, sink := range j.sinks {
		if sink(final) {
			reached = true
			delete(j.pending, c) // this connection is caught up
		} else {
			if j.pending == nil {
				j.pending = map[*conn]map[string]any{}
			}
			j.pending[c] = final
		}
	}
	// Reached a live connection ⇒ no orphan (a reconnecting device reloads history);
	// reached nobody ⇒ keep it for the next attach so the reply isn't lost.
	if reached {
		j.orphan = nil
	} else {
		j.orphan = final
	}
}

// flushPending redelivers each connection's buffered-but-undelivered reply from an
// earlier turn (its send failed because that connection was momentarily
// unreachable) now that we are about to write again — e.g. at the next turn's
// "thinking" ping, by which point a backgrounded/stalled socket has typically
// recovered. Each buffer is retried against ITS OWN connection; one that succeeds
// clears only that connection's buffer. A connection that has since detached is
// dropped (it reloads history on reconnect). Call WITHOUT j.mu held.
func (j *sessionJob) flushPending() {
	j.mu.Lock()
	defer j.mu.Unlock()
	for c, msg := range j.pending {
		sink := j.sinks[c]
		if sink == nil {
			delete(j.pending, c) // detached — it reloads from the transcript on reconnect
			continue
		}
		if sink(msg) {
			delete(j.pending, c)
		}
	}
}

// beginTurn moves the job into the running state for a new turn or compress. The
// per-connection pending buffers and the orphan buffer are LEFT INTACT — they hold
// only still-undelivered replies (deliver clears each buffer the moment its own
// connection receives it), so flushPending can redeliver them; this turn's own
// deliver overwrites them when it finishes. Call with j.mu held.
func (j *sessionJob) beginTurn(cancel context.CancelFunc) {
	j.running = true
	j.aborted = false
	j.cancel = cancel
	j.turnFrames = nil // fresh replay buffer for this turn's streamed output
}

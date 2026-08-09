package session

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// StageTimer collects where the wall-clock of one display-history read actually
// went — the SSH chain stats, the transcript read+parse, and whether the digest
// came from cache — so the gateway can report an attach's latency breakdown
// instead of a single opaque total.
//
// It rides on the context rather than the call signatures on purpose: the stages
// live inside Driver.DisplayHistory / DisplayDigest, several layers below the
// caller that wants the numbers, and every other caller of those methods (the
// digest sweep, the turn path) wants nothing. A nil timer — the case for every
// uninstrumented caller — makes every method here a no-op, so the seam costs
// nothing when it isn't used.
type StageTimer struct {
	mu     sync.Mutex
	order  []string
	stages map[string]time.Duration
	notes  []string
}

type stageTimerKey struct{}

// NewStageTimer returns a timer ready to collect stages.
func NewStageTimer() *StageTimer {
	return &StageTimer{stages: make(map[string]time.Duration)}
}

// WithStageTimer attaches t to ctx so the driver's stages report into it.
func WithStageTimer(ctx context.Context, t *StageTimer) context.Context {
	return context.WithValue(ctx, stageTimerKey{}, t)
}

// StageTimerFrom returns the timer attached to ctx, or nil.
func StageTimerFrom(ctx context.Context) *StageTimer {
	t, _ := ctx.Value(stageTimerKey{}).(*StageTimer)
	return t
}

// Add records d against a named stage, accumulating repeats of the same name.
func (t *StageTimer) Add(stage string, d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, seen := t.stages[stage]; !seen {
		t.order = append(t.order, stage)
	}
	t.stages[stage] += d
}

// Note records a non-timing fact about the read (e.g. "digest=cached").
func (t *StageTimer) Note(note string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.notes = append(t.notes, note)
}

// String renders the collected stages in the order they were first seen, e.g.
// "chain=12ms parse=340ms digest=cached".
func (t *StageTimer) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := make([]string, 0, len(t.order)+len(t.notes))
	for _, name := range t.order {
		parts = append(parts, fmt.Sprintf("%s=%v", name, t.stages[name].Round(time.Millisecond)))
	}
	parts = append(parts, t.notes...)
	return strings.Join(parts, " ")
}

// timeStage times the enclosing block into ctx's timer: `defer timeStage(ctx, "chain")()`.
func timeStage(ctx context.Context, stage string) func() {
	t := StageTimerFrom(ctx)
	if t == nil {
		return func() {}
	}
	started := time.Now()
	return func() { t.Add(stage, time.Since(started)) }
}

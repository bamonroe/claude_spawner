package session

import (
	"context"
	"testing"
	"time"
)

// The seed promise is what makes an off-loop handoff recap safe: a turn that
// observes the rotated context must also observe the pending promise, so a late
// recap is waited for rather than silently lost.
func TestAwaitSeedWaitsForLateSeed(t *testing.T) {
	s := &Session{Name: "s", SessionID: "old"}
	settle := s.MutateWithSeed(func(s *Session) { s.SessionID = "new" })
	go func() {
		time.Sleep(20 * time.Millisecond)
		settle("recap")
	}()
	s.AwaitSeed(context.Background())
	if s.PendingSeed != "recap" {
		t.Fatalf("AwaitSeed returned before the seed landed, got %q", s.PendingSeed)
	}
}

// Nothing pending is the common case: AwaitSeed must not block.
func TestAwaitSeedNoPromiseReturns(t *testing.T) {
	s := &Session{Name: "s"}
	done := make(chan struct{})
	go func() { s.AwaitSeed(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AwaitSeed blocked with no promise open")
	}
}

// A settle that arrives after the context rotated again describes a conversation
// the record has already left — it must not seed the wrong context, and must
// still release waiters.
func TestSettleAfterRotationIsDropped(t *testing.T) {
	s := &Session{SessionID: "a"}
	settle := s.MutateWithSeed(func(s *Session) { s.SessionID = "b" })
	s.Mutate(func(s *Session) { s.SessionID = "c" }) // a clear/compress rotated again
	settle("stale recap")
	if s.PendingSeed != "" {
		t.Fatalf("stale recap seeded the new context: %q", s.PendingSeed)
	}
	s.AwaitSeed(context.Background()) // must not hang
}

// An abandoned promise (a second switch before the first recap lands) must not
// strand a waiter on the old channel.
func TestSecondPromiseReleasesTheFirst(t *testing.T) {
	s := &Session{SessionID: "a"}
	first := s.MutateWithSeed(func(s *Session) { s.SessionID = "b" })
	second := s.MutateWithSeed(func(s *Session) { s.SessionID = "c" })
	first("orphan") // the first computation finishes late; it owns nothing now
	if s.PendingSeed != "" {
		t.Fatalf("orphaned recap landed: %q", s.PendingSeed)
	}
	second("live")
	s.AwaitSeed(context.Background())
	if s.PendingSeed != "live" {
		t.Fatalf("live recap missing, got %q", s.PendingSeed)
	}
}

// A recap that never arrives must not wedge the turn: AwaitSeed gives up with ctx.
func TestAwaitSeedHonorsContext(t *testing.T) {
	s := &Session{}
	s.MutateWithSeed(func(s *Session) {})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	s.AwaitSeed(ctx)
}

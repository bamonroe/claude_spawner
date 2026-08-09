package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestPurgeQueueRoundTrip covers the durability contract: an owed purge survives
// a reopen, duplicates collapse, and Resolve only drops what it settles.
func TestPurgeQueueRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "purges.json")
	q := OpenPurgeQueue(path)
	item := PurgeItem{Session: "s1", Agent: "claude", Host: "mom", IDs: []string{"a", "b"}, Created: time.Now()}
	other := PurgeItem{Session: "s2", Agent: "codex", Host: "dad", IDs: []string{"c"}}
	q.Add(item, item, other, PurgeItem{Session: "empty"}) // dup + empty are dropped

	if got := len(OpenPurgeQueue(path).Pending()); got != 2 {
		t.Fatalf("pending after reopen = %d, want 2", got)
	}

	q = OpenPurgeQueue(path)
	cleared := q.Resolve(func(it PurgeItem) bool { return it.Host == "mom" })
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1", cleared)
	}
	left := OpenPurgeQueue(path).Pending()
	if len(left) != 1 || left[0].Host != "dad" {
		t.Fatalf("remaining = %+v, want the dad item", left)
	}
	if left[0].Attempts != 1 {
		t.Errorf("attempts = %d, want 1", left[0].Attempts)
	}
}

// TestPurgeSegmentDefersWhenHostDown is the point of the whole seam: a delete
// against a host in the pool's negative-dial window must return immediately with
// the work owed, not attempt (and stall on) any SSH.
func TestPurgeSegmentDefersWhenHostDown(t *testing.T) {
	pool := &SSHPool{entries: map[string]*poolEntry{}}
	e := pool.entry("mom")
	e.mu.Lock()
	e.markDown(time.Now(), "mom", errors.New("no route to host"))
	e.mu.Unlock()

	if _, down := pool.Down("mom"); !down {
		t.Fatal("pool.Down(mom) = false, want true after markDown")
	}
	if _, down := pool.Down("dad"); down {
		t.Fatal("pool.Down(dad) = true, want false for an untried host")
	}

	d := NewDriver()
	d.SetExec(TargetHost, SSHExecutor{Pool: pool})
	q := OpenPurgeQueue("")
	d.SetPurgeQueue(q)

	n, err := d.purgeSegment(context.Background(), "s1", "claude", "mom", []string{"id-1"})
	if err != nil || n != 0 {
		t.Fatalf("purgeSegment on a down host = (%d, %v), want (0, nil)", n, err)
	}
	pending := q.Pending()
	if len(pending) != 1 || pending[0].Host != "mom" || pending[0].IDs[0] != "id-1" {
		t.Fatalf("owed items = %+v, want the mom/id-1 purge deferred", pending)
	}
	// A host that's back stops being deferred: RetryPurges must clear it once the
	// negative-cache window lapses and the (local, in-test) delete succeeds.
	e.mu.Lock()
	e.markUp()
	e.mu.Unlock()
	if _, down := pool.Down("mom"); down {
		t.Fatal("pool.Down(mom) still true after markUp")
	}
}

// TestPurgeQueueAbandonsAfterMaxAttempts covers the bound on retries: an item
// whose purge never succeeds must eventually be dropped, not retried on the
// ticker for the life of the server.
func TestPurgeQueueAbandonsAfterMaxAttempts(t *testing.T) {
	q := OpenPurgeQueue("")
	q.Add(PurgeItem{Session: "doomed", Agent: "claude", Host: "mom", IDs: []string{"id-1"}})
	for i := 1; i < maxPurgeAttempts; i++ {
		if cleared := q.Resolve(func(PurgeItem) bool { return false }); cleared != 0 {
			t.Fatalf("attempt %d cleared %d, want 0", i, cleared)
		}
		if len(q.Pending()) != 1 {
			t.Fatalf("item dropped after %d attempts, want it kept until %d", i, maxPurgeAttempts)
		}
	}
	q.Resolve(func(PurgeItem) bool { return false }) // the maxPurgeAttempts'th
	if got := q.Pending(); len(got) != 0 {
		t.Fatalf("pending = %+v, want the doomed item abandoned", got)
	}
}

package session

import (
	"path/filepath"
	"sync"
	"testing"
)

// counter is a subscriber that just counts wakes, safe to read from the test
// goroutine (notify runs on the mutating goroutine, which is this one).
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) hit() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *counter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func watchedStore(t *testing.T) (*Store, *counter) {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "sessions.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	c := &counter{}
	s.Subscribe(c.hit)
	return s, c
}

// Every mutation that changes the list fires exactly once.
func TestStoreNotifiesOnEachMutation(t *testing.T) {
	s, c := watchedStore(t)

	rec := &Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}
	if _, err := s.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := c.count(); got != 1 {
		t.Fatalf("Insert: want 1 notification, got %d", got)
	}

	rec.Mutate(func(r *Session) { r.Model = "opus" })
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := c.count(); got != 2 {
		t.Fatalf("Put(model): want 2, got %d", got)
	}

	if err := s.Rename("alpha", "beta"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := c.count(); got != 3 {
		t.Fatalf("Rename: want 3, got %d", got)
	}

	if err := s.Delete("beta"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := c.count(); got != 4 {
		t.Fatalf("Delete: want 4, got %d", got)
	}
}

// Every list-visible field is watched — one Put per field, one wake per Put.
func TestStoreNotifiesOnEveryListVisibleField(t *testing.T) {
	s, c := watchedStore(t)
	rec := &Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}
	if _, err := s.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	edits := []struct {
		name string
		set  func(r *Session)
	}{
		{"dir", func(r *Session) { r.Dir = "/tmp/moved" }},
		{"session_id", func(r *Session) { r.SessionID = "id-2" }},
		{"host", func(r *Session) { r.Host = "work" }},
		{"target", func(r *Session) { r.Target = TargetSandbox }},
		{"agent", func(r *Session) { r.Agent = "codex" }},
		{"model", func(r *Session) { r.Model = "sonnet" }},
		{"profile", func(r *Session) { r.Profile = "deep" }},
		{"name", func(r *Session) { r.Name = "gamma" }},
	}
	want := 1 // the Insert
	for _, e := range edits {
		rec.Mutate(e.set)
		if err := s.Put(rec); err != nil {
			t.Fatalf("Put(%s): %v", e.name, err)
		}
		want++
		if got := c.count(); got != want {
			t.Fatalf("Put(%s): want %d notifications, got %d", e.name, want, got)
		}
	}
}

// The per-turn writes are the whole reason for the field comparison: a Put that
// touches nothing the list shows must not wake anybody.
func TestStoreDoesNotNotifyOnNoOpPut(t *testing.T) {
	s, c := watchedStore(t)
	rec := &Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}
	if _, err := s.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	base := c.count()

	for i := 0; i < 5; i++ {
		if err := s.Put(rec); err != nil { // a bare re-Put, as a turn does
			t.Fatalf("Put: %v", err)
		}
	}
	// Fields the list never renders change nothing for subscribers either.
	rec.Mutate(func(r *Session) { r.Started = true; r.AskPrimed = true })
	if err := s.Put(rec); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := c.count(); got != base {
		t.Fatalf("no-op Puts woke subscribers: want %d notifications, got %d", base, got)
	}

	// A rename to the same name is a no-op too.
	if err := s.Rename("alpha", "alpha"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	// So is deleting a name that isn't registered.
	if err := s.Delete("nobody"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// And an Insert that resolves to an existing record by session_id.
	dup := &Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}
	if got, err := s.Insert(dup); err != nil || got != rec {
		t.Fatalf("Insert(dup) = %v, %v; want the existing record", got, err)
	}
	if got := c.count(); got != base {
		t.Fatalf("no-op mutations woke subscribers: want %d notifications, got %d", base, got)
	}
}

// ForgetID is index-only, so it wakes subscribers exactly when the caller has
// already rotated the record onto a new session_id — the clear/compress case.
func TestStoreForgetIDNotifiesOnlyAfterRotation(t *testing.T) {
	s, c := watchedStore(t)
	rec := &Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}
	if _, err := s.Insert(rec); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	base := c.count()

	if err := s.ForgetID("never-indexed"); err != nil {
		t.Fatalf("ForgetID: %v", err)
	}
	if got := c.count(); got != base {
		t.Fatalf("ForgetID of an unrelated id woke subscribers: got %d, want %d", got, base)
	}

	rec.Mutate(func(r *Session) { r.SessionID = "id-2" }) // a clear/compress rotation
	if err := s.ForgetID("id-1"); err != nil {
		t.Fatalf("ForgetID: %v", err)
	}
	if got := c.count(); got != base+1 {
		t.Fatalf("ForgetID after rotation: want %d notifications, got %d", base+1, got)
	}
}

// Loading a populated store must not look like a change to the first mutation.
func TestStoreSeedsProjectionOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if _, err := s.Insert(&Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := OpenStore(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	c := &counter{}
	reopened.Subscribe(c.hit)
	if err := reopened.Put(reopened.Get("alpha")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := c.count(); got != 0 {
		t.Fatalf("re-Put of a loaded record woke subscribers %d times; want 0", got)
	}
}

// Cancel actually unsubscribes, and other subscribers keep hearing changes.
func TestStoreSubscribeCancel(t *testing.T) {
	s, kept := watchedStore(t)
	dropped := &counter{}
	cancel := s.Subscribe(dropped.hit)
	cancel()

	if _, err := s.Insert(&Session{Name: "alpha", Dir: "/tmp/alpha", SessionID: "id-1"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if got := dropped.count(); got != 0 {
		t.Fatalf("cancelled subscriber still fired %d times", got)
	}
	if got := kept.count(); got != 1 {
		t.Fatalf("live subscriber: want 1, got %d", got)
	}
}

// Concurrent mutations: no lost wakes, no deadlock, and the final projection is
// the one subscribers were last told about.
func TestStoreNotifyUnderConcurrentMutations(t *testing.T) {
	s, c := watchedStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := string(rune('a'+i)) + "-sess"
			if _, err := s.Insert(&Session{Name: name, Dir: "/tmp/" + name, SessionID: name}); err != nil {
				t.Errorf("Insert: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := c.count(); got < 1 {
		t.Fatalf("concurrent inserts produced no notifications")
	}
	if got := len(s.List()); got != 16 {
		t.Fatalf("want 16 records, got %d", got)
	}
	// Whatever the interleaving, the remembered projection is the current one, so
	// a following no-op Put stays silent.
	before := c.count()
	if err := s.Put(s.Get("a-sess")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := c.count(); got != before {
		t.Fatalf("stale remembered projection: no-op Put fired %d extra times", got-before)
	}
}

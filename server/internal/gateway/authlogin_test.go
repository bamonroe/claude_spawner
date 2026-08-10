package gateway

import (
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The registry, not the PTY: these cover the single-flight invariant itself —
// one live login per host, joins instead of races, and no stranded slot when the
// last client watching it disappears.

func newLoginServer() *Server {
	return &Server{logins: map[string]*pendingLogin{}}
}

func TestClaimLoginJoinsPending(t *testing.T) {
	s := newLoginServer()
	a, b := &conn{}, &conn{}

	first, existing := s.claimLogin(a, "box", "")
	if first == nil || existing != nil {
		t.Fatalf("first claim should own the slot, got mine=%v existing=%v", first, existing)
	}
	mine, joined := s.claimLogin(b, "box", "")
	if mine != nil || joined != first {
		t.Fatalf("second claim should join the pending login, got mine=%v existing=%v", mine, joined)
	}
	if got := s.loginFor("box"); got != first {
		t.Fatalf("registry holds %v, want the first claim", got)
	}
}

func TestClaimLoginReplacesOnDifferentIdentity(t *testing.T) {
	s := newLoginServer()
	first, _ := s.claimLogin(&conn{}, "box", session.AuthLoginClaudeAI)
	second, existing := s.claimLogin(&conn{}, "box", session.AuthLoginConsole)
	if existing != nil {
		t.Fatalf("a different identity must not join, got existing=%v", existing)
	}
	if second == first {
		t.Fatal("a different identity must claim a fresh slot")
	}
	if got := s.loginFor("box"); got != second {
		t.Fatalf("registry holds %v, want the replacement", got)
	}
}

func TestClaimLoginPerHost(t *testing.T) {
	s := newLoginServer()
	a, _ := s.claimLogin(&conn{}, "box", "")
	b, existing := s.claimLogin(&conn{}, "other", "")
	if existing != nil || b == nil || a == b {
		t.Fatal("logins on different hosts are independent")
	}
}

func TestReleaseLoginsKeepsSlotWhileWatched(t *testing.T) {
	s := newLoginServer()
	a, b := &conn{}, &conn{}
	p, _ := s.claimLogin(a, "box", "")
	s.claimLogin(b, "box", "")

	s.releaseLogins(a)
	if s.loginFor("box") != p {
		t.Fatal("a login still watched by another client must survive a disconnect")
	}
	s.releaseLogins(b)
	if got := s.loginFor("box"); got != nil {
		t.Fatalf("last watcher gone should clear the slot, got %v", got)
	}
}

func TestDropLoginOnlyClearsCurrent(t *testing.T) {
	s := newLoginServer()
	stale, _ := s.claimLogin(&conn{}, "box", session.AuthLoginClaudeAI)
	live, _ := s.claimLogin(&conn{}, "box", session.AuthLoginConsole)

	s.dropLogin("box", stale) // a superseded attempt cleaning up
	if got := s.loginFor("box"); got != live {
		t.Fatalf("superseded cleanup evicted the live login: %v", got)
	}
	s.dropLogin("box", live)
	if got := s.loginFor("box"); got != nil {
		t.Fatalf("current login should be cleared, got %v", got)
	}
}

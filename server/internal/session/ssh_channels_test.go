package session

import (
	"errors"
	"fmt"
	"testing"

	"golang.org/x/crypto/ssh"
)

// TestShouldRedial pins the pool's drop policy. The two false cases are the ones
// that cost a live turn: a busy connection and a remote command that merely
// exited non-zero both look like errors, and dropping the client over either
// tears down every other channel on it.
func TestShouldRedial(t *testing.T) {
	if shouldRedial(nil) {
		t.Error("no error should never earn a re-dial")
	}
	if shouldRedial(fmt.Errorf("%w: too many sessions", errChannelOpen)) {
		t.Error("a channel-open refusal means busy, not dead — must not re-dial")
	}
	if shouldRedial(&ssh.ExitError{}) {
		t.Error("a remote command's exit status means the connection worked — must not re-dial")
	}
	if shouldRedial(fmt.Errorf("wrapped: %w", &ssh.ExitError{})) {
		t.Error("a wrapped exit status must still not re-dial")
	}
	if !shouldRedial(errors.New("connection reset by peer")) {
		t.Error("a transport error must earn a re-dial")
	}
}

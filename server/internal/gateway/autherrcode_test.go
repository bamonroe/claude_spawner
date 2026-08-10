package gateway

import (
	"errors"
	"testing"
)

func TestTurnFailureCode(t *testing.T) {
	if got := turnFailureCode(errors.New("boom")); got != "turn_failed" {
		t.Errorf("ordinary failure = %q, want turn_failed", got)
	}
	err := errors.New("claude exited 1: OAuth token has expired")
	if got := turnFailureCode(err); got != "turn_failed_auth" {
		t.Errorf("credential failure = %q, want turn_failed_auth", got)
	}
	if spokenError["turn_failed_auth"] == "" {
		t.Error("turn_failed_auth has no spoken rendering")
	}
}

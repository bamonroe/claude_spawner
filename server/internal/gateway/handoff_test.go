package gateway

import (
	"strings"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// formatHandoffRecap renders a backend's transcript into the neutral, bounded
// dialogue that seeds the next backend on an AI switch (doSetAgent). These tests
// pin the behavior the handoff relies on: neutral role labels, chronological order,
// skipping empties, a recent-weighted budget with an elision marker, and an empty
// recap when there's nothing to carry (so a switch off a null-transcript backend
// stays clean).
func TestFormatHandoffRecap(t *testing.T) {
	msgs := []session.Message{
		{Role: "user", Text: "  hello there  "},
		{Role: "claude", Text: "hi, working on the parser"},
		{Role: "user", Text: ""}, // dropped
		{Role: "user", Text: "what's next"},
	}
	got := formatHandoffRecap(msgs)
	want := "User: hello there\n\nAssistant: hi, working on the parser\n\nUser: what's next"
	if got != want {
		t.Fatalf("recap mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestFormatHandoffRecapEmpty(t *testing.T) {
	if got := formatHandoffRecap(nil); got != "" {
		t.Fatalf("nil transcript: want empty recap, got %q", got)
	}
	blank := []session.Message{{Role: "user", Text: "   "}, {Role: "claude", Text: ""}}
	if got := formatHandoffRecap(blank); got != "" {
		t.Fatalf("blank transcript: want empty recap, got %q", got)
	}
}

func TestFormatHandoffRecapBudgetKeepsNewest(t *testing.T) {
	big := strings.Repeat("x", handoffRecapBudget)
	msgs := []session.Message{
		{Role: "user", Text: "OLDEST-" + big},
		{Role: "claude", Text: "MID-" + big},
		{Role: "user", Text: "NEWEST question"},
	}
	got := formatHandoffRecap(msgs)
	if !strings.HasPrefix(got, "[…earlier conversation elided…]") {
		t.Fatalf("expected elision marker, got prefix %q", got[:min(40, len(got))])
	}
	if !strings.Contains(got, "NEWEST question") {
		t.Fatalf("newest message must survive the budget: %q", got)
	}
	if strings.Contains(got, "OLDEST") {
		t.Fatalf("oldest over-budget message should be elided: %q", got)
	}
}

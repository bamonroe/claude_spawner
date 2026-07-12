package gateway

import (
	"strings"
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

func TestCapTail(t *testing.T) {
	if got := capTail(""); got != "" {
		t.Errorf("empty tail: %q", got)
	}
	// Line cap: keep only the last jobTailMaxLines, prefixed with a trim marker.
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, "line")
	}
	got := capTail(strings.Join(lines, "\n"))
	n := len(strings.Split(got, "\n"))
	if n > jobTailMaxLines+1 { // +1 for the trim marker line
		t.Errorf("line cap: got %d lines", n)
	}
	if !strings.Contains(got, "trimmed") {
		t.Errorf("expected trim marker, got %q", got)
	}
	// Rune cap: a single huge line is trimmed from the left.
	big := strings.Repeat("x", jobNoteMaxRunes*2)
	got = capTail(big)
	if len([]rune(got)) > jobNoteMaxRunes+1 { // +1 for the leading ellipsis
		t.Errorf("rune cap: got %d runes", len([]rune(got)))
	}
}

func TestJobNote(t *testing.T) {
	n := jobNote("go build ./...", "ok\ndone")
	if !strings.Contains(n, "go build") || !strings.Contains(n, "finished") || !strings.Contains(n, "ok") {
		t.Errorf("jobNote missing parts: %q", n)
	}
	// No tail -> no "Last output" section.
	n = jobNote("sleep 5", "")
	if strings.Contains(n, "Last output") {
		t.Errorf("empty tail should omit output section: %q", n)
	}
}

func TestTrimToJSONArray(t *testing.T) {
	cases := map[string]string{
		`[{"id":"a"}]`:                   `[{"id":"a"}]`,
		"warning: foo\n[{\"id\":\"a\"}]": `[{"id":"a"}]`,
		"[]":                             "[]",
		"no array here":                  "no array here",
	}
	for in, want := range cases {
		if got := string(trimToJSONArray([]byte(in))); got != want {
			t.Errorf("trimToJSONArray(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDropJobByID(t *testing.T) {
	jobs := []session.BackgroundJob{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	got := dropJobByID(jobs, "b")
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("dropJobByID: %+v", got)
	}
}

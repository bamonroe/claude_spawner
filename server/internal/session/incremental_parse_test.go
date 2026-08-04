package session

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// convo builds a realistic run of transcript lines: a dictation, a tool-only
// assistant line, and the closing assistant text line that carries the usage —
// the shape the turn rollup (Turns/TurnTotal) is computed from.
func convo(n int) []string {
	return []string{
		fmt.Sprintf(`{"type":"user","uuid":"u%d","timestamp":"2026-08-01T00:0%d:00Z","message":{"content":"ask %d"}}`, n, n, n),
		fmt.Sprintf(`{"type":"assistant","uuid":"a%da","timestamp":"2026-08-01T00:0%d:01Z","message":{"content":[{"type":"tool_use"}],"usage":{"input_tokens":%d,"output_tokens":1,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`, n, n, 100*n),
		fmt.Sprintf(`{"type":"assistant","uuid":"a%db","timestamp":"2026-08-01T00:0%d:02Z","message":{"content":[{"type":"text","text":"reply %d"}],"usage":{"input_tokens":%d,"output_tokens":5,"cache_read_input_tokens":7,"cache_creation_input_tokens":0}}}`, n, n, n, 200*n),
	}
}

// writeLines writes lines (each newline-terminated) to path, truncating.
func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, '\n')
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fullParse parses the same content at a FRESH path, so it can't hit the
// incremental cache — the reference answer to compare against.
func fullParse(t *testing.T, lines []string) []Message {
	t.Helper()
	p := filepath.Join(t.TempDir(), "reference.jsonl")
	writeLines(t, p, lines)
	msgs, err := localClaudeFS.readTranscript(p)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// TestIncrementalParse_MatchesFullParseAcrossAppends is the invariant the whole
// incremental cache rests on: growing a transcript one turn at a time must yield
// exactly what parsing the finished file from scratch yields — including the turn
// rollup, which stays OPEN across an append boundary until the next dictation
// closes it.
func TestIncrementalParse_MatchesFullParseAcrossAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live.jsonl")
	var lines []string
	for n := 1; n <= 4; n++ {
		lines = append(lines, convo(n)...)
		writeLines(t, path, lines)
		got, err := localClaudeFS.readTranscript(path)
		if err != nil {
			t.Fatalf("append %d: %v", n, err)
		}
		want := fullParse(t, lines)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after append %d:\n got %+v\nwant %+v", n, got, want)
		}
		if len(got) != 2*n {
			t.Fatalf("after append %d: %d messages, want %d", n, len(got), 2*n)
		}
	}
}

// TestIncrementalParse_AppendMidLine covers a turn caught mid-write: a trailing
// partial line must be left unconsumed, not parsed as garbage and skipped
// forever, so it appears once the rest of it lands.
func TestIncrementalParse_AppendMidLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "midwrite.jsonl")
	lines := convo(1)
	writeLines(t, path, lines)
	if msgs, _ := localClaudeFS.readTranscript(path); len(msgs) != 2 {
		t.Fatalf("baseline: %d messages, want 2", len(msgs))
	}

	// Append the next turn's first line WITHOUT its newline.
	next := convo(2)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(next[0]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if msgs, _ := localClaudeFS.readTranscript(path); len(msgs) != 2 {
		t.Fatalf("mid-write: %d messages, want the partial line ignored (2)", len(msgs))
	}

	// Complete it and add the rest of the turn.
	writeLines(t, path, append(lines, next...))
	got, err := localClaudeFS.readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	want := fullParse(t, append(lines, next...))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after completion:\n got %+v\nwant %+v", got, want)
	}
}

// TestIncrementalParse_DetectsRewrite guards the append-only assumption: a file
// rewritten in place (not appended to) must fall back to a full re-parse rather
// than splicing new bytes onto a stale prefix.
func TestIncrementalParse_DetectsRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rewritten.jsonl")
	writeLines(t, path, append(convo(1), convo(2)...))
	if msgs, _ := localClaudeFS.readTranscript(path); len(msgs) != 4 {
		t.Fatalf("baseline: %d messages, want 4", len(msgs))
	}

	// Replace the contents with a LONGER but entirely different transcript, so the
	// size grew (which is what would tempt an incremental extension).
	rewritten := append(append(convo(7), convo(8)...), convo(9)...)
	writeLines(t, path, rewritten)
	got, err := localClaudeFS.readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	want := fullParse(t, rewritten)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after rewrite:\n got %+v\nwant %+v", got, want)
	}
}

// TestIncrementalParse_Truncation covers the other non-append edit: a file that
// shrank must be re-parsed from scratch.
func TestIncrementalParse_Truncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "truncated.jsonl")
	writeLines(t, path, append(convo(1), convo(2)...))
	if msgs, _ := localClaudeFS.readTranscript(path); len(msgs) != 4 {
		t.Fatalf("baseline: %d messages, want 4", len(msgs))
	}
	writeLines(t, path, convo(1))
	got, err := localClaudeFS.readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := fullParse(t, convo(1)); !reflect.DeepEqual(got, want) {
		t.Fatalf("after truncation:\n got %+v\nwant %+v", got, want)
	}
}

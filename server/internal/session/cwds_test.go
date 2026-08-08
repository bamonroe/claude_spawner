package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cwds must recover the working directory without reading the whole transcript —
// the remote backend pays for every byte. A file whose head carries the cwd but
// whose tail is enormous exercises the bound.
func TestCwdsReadsOnlyABoundedHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	var b strings.Builder
	b.WriteString(`{"cwd":"/data/claude_spawner"}` + "\n")
	for i := 0; i < 5000; i++ {
		b.WriteString(`{"filler":"` + strings.Repeat("x", 200) + `"}` + "\n")
	}
	if len(b.String()) <= cwdHeadBytes {
		t.Fatalf("test file (%d bytes) must exceed the %d-byte head budget", len(b.String()), cwdHeadBytes)
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localClaudeFS.cwds(context.Background(), []string{path})[path]; got != "/data/claude_spawner" {
		t.Fatalf("cwds = %q, want /data/claude_spawner", got)
	}
}

// A single line longer than the head budget is why the bound is in bytes, not
// lines: a 40-line budget would have pulled megabytes per file. A cwd buried
// past the budget is simply not found — that's the deliberate trade.
func TestCwdsBoundIsBytesNotLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge-first-line.jsonl")
	body := `{"toolResult":"` + strings.Repeat("y", cwdHeadBytes+1024) + `"}` + "\n" +
		`{"cwd":"/home/bam"}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := localClaudeFS.cwds(context.Background(), []string{path})[path]; got != "" {
		t.Fatalf("cwds = %q, want empty (cwd sits past the head budget)", got)
	}
}

// Missing files and cwd-less transcripts are absent from the result rather than
// erroring — discovery skips them.
func TestCwdsSkipsMissingAndCwdlessFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone.jsonl")
	nocwd := filepath.Join(dir, "nocwd.jsonl")
	if err := os.WriteFile(nocwd, []byte("{\"type\":\"summary\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localClaudeFS.cwds(context.Background(), []string{missing, nocwd})
	if _, ok := got[missing]; ok {
		t.Fatal("missing file should not appear in the result")
	}
	if _, ok := got[nocwd]; ok {
		t.Fatal("cwd-less file should not appear in the result")
	}
}

// cwdFromMatch unwraps what the remote extractor's grep returns, and rejects
// anything that isn't a clean, escape-free match.
func TestCwdFromMatch(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`"cwd":"/data/claude_spawner"`, "/data/claude_spawner"},
		{`"cwd":"/data/claude_spawner"` + "\r", "/data/claude_spawner"},
		{`"cwd":""`, ""},
		{"", ""},
		{`garbage`, ""},
		{`"cwd":"/unterminated`, ""},
	} {
		if got := cwdFromMatch(tc.in); got != tc.want {
			t.Errorf("cwdFromMatch(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

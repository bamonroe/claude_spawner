package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heads must read only the head of a file — never the whole thing — because the
// remote backend pays for every byte it pulls. A transcript with a huge tail
// exercises that: the returned head is bounded by the line limit.
func TestHeadsReadsOnlyTheHead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	var b strings.Builder
	b.WriteString(`{"cwd":"/data/claude_spawner"}` + "\n")
	for i := 0; i < 5000; i++ {
		b.WriteString(`{"filler":"` + strings.Repeat("x", 200) + `"}` + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localClaudeFS.heads([]string{path}, cwdHeadLines)[path]
	if n := strings.Count(string(got), "\n"); n > cwdHeadLines {
		t.Fatalf("heads returned %d lines, want at most %d", n, cwdHeadLines)
	}
	if cwd := cwdFromHead(got); cwd != "/data/claude_spawner" {
		t.Fatalf("cwdFromHead = %q, want /data/claude_spawner", cwd)
	}
}

// A path that doesn't exist is simply absent from the result, and yields no cwd.
func TestHeadsSkipsMissingAndCwdlessFiles(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "gone.jsonl")
	nocwd := filepath.Join(dir, "nocwd.jsonl")
	if err := os.WriteFile(nocwd, []byte("{\"type\":\"summary\"}\nnot json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := localClaudeFS.heads([]string{missing, nocwd}, cwdHeadLines)
	if _, ok := got[missing]; ok {
		t.Fatal("missing file should not appear in the result")
	}
	if cwd := cwdFromHead(got[nocwd]); cwd != "" {
		t.Fatalf("cwdFromHead = %q, want empty", cwd)
	}
}

package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscriptLines writes lines to a temp .jsonl and returns its path.
func writeTranscriptLines(t *testing.T, lines []string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func usageLine(input, output, ts string) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":%s,"output_tokens":%s,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		ts, input, output)
}

// bulkLine is a single enormous transcript line, the shape that makes a byte
// budget (rather than a line count) the right bound.
func bulkLine(n int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"content": strings.Repeat("x", n),
		},
	})
	return string(b)
}

// TestLastUsageInFile_ReadsFromTheTail is the core of the bounded-tail read: the
// answer is the LAST usage-bearing line, not the first, and earlier ones don't win.
func TestLastUsageInFile_ReadsFromTheTail(t *testing.T) {
	p := writeTranscriptLines(t, []string{
		usageLine("100", "1", "2026-08-01T00:00:00Z"),
		`{"type":"user","message":{"content":"go on"}}`,
		usageLine("900", "2", "2026-08-01T00:01:00Z"),
	})
	snap := localClaudeFS.lastUsageInFile(p)
	if snap == nil {
		t.Fatal("no snapshot")
	}
	if snap.Usage.Input != 900 || snap.Usage.Output != 2 {
		t.Fatalf("got %+v, want the newest turn (input 900)", snap.Usage)
	}
}

// TestLastUsageInFile_WidensPastHugeTrailingLines confirms the budget escalation:
// when the tail window lands entirely inside one giant tool-result line, the read
// widens instead of reporting "no usage".
func TestLastUsageInFile_WidensPastHugeTrailingLines(t *testing.T) {
	p := writeTranscriptLines(t, []string{
		usageLine("777", "3", "2026-08-01T00:00:00Z"),
		bulkLine(64 << 10),
	})
	orig := usageTailBudgets
	usageTailBudgets = []int64{1 << 10, 4 << 10, 1 << 20} // force two escalations
	defer func() { usageTailBudgets = orig }()

	snap := localClaudeFS.lastUsageInFile(p)
	if snap == nil {
		t.Fatal("no snapshot: the read failed to widen past the trailing bulk line")
	}
	if snap.Usage.Input != 777 {
		t.Fatalf("got %+v, want input 777", snap.Usage)
	}
}

// TestLastUsageInFile_NoUsage returns nil rather than escalating forever on a
// transcript that has no usage-bearing assistant line at all.
func TestLastUsageInFile_NoUsage(t *testing.T) {
	p := writeTranscriptLines(t, []string{`{"type":"user","message":{"content":"hello"}}`})
	if snap := localClaudeFS.lastUsageInFile(p); snap != nil {
		t.Fatalf("got %+v, want nil", snap)
	}
}

// TestTailBytes_WholeFileFlag guards the flag the caller uses to decide whether
// its first line is a truncated fragment.
func TestTailBytes_WholeFileFlag(t *testing.T) {
	p := writeTranscriptLines(t, []string{"aaaa", "bbbb"})
	if data, whole, err := localClaudeFS.tailBytes(p, 1<<20); err != nil || !whole || string(data) != "aaaa\nbbbb\n" {
		t.Fatalf("full read: data=%q whole=%v err=%v", data, whole, err)
	}
	data, whole, err := localClaudeFS.tailBytes(p, 5)
	if err != nil || whole || string(data) != "bbbb\n" {
		t.Fatalf("bounded read: data=%q whole=%v err=%v", data, whole, err)
	}
}

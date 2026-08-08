package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// codexTurn builds one realistic Codex rollout turn: the user's message, the
// agent's reply, the response_item that carries the reply's durable msg_… id, and
// the token_count that badges it. The id and the usage both land AFTER the
// agent_message they belong to, which is exactly the mid-scan state an incremental
// parse has to carry across an append boundary.
func codexTurn(n int) []string {
	return []string{
		fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-08-01T00:0%d:00Z","payload":{"type":"user_message","message":"ask %d"}}`, n, n),
		fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-08-01T00:0%d:01Z","payload":{"type":"agent_message","message":"reply %d"}}`, n, n),
		fmt.Sprintf(`{"type":"response_item","timestamp":"2026-08-01T00:0%d:01Z","payload":{"type":"message","role":"assistant","id":"msg_%d","content":[{"text":"reply %d"}]}}`, n, n, n),
		fmt.Sprintf(`{"type":"event_msg","timestamp":"2026-08-01T00:0%d:02Z","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":5,"reasoning_output_tokens":1}}}}`, n, 100*n, 10*n),
	}
}

// codexFullParse parses the same content at a FRESH path so it can't hit the
// incremental cache — the reference answer.
func codexFullParse(t *testing.T, lines []string) []Message {
	t.Helper()
	cfs := codexFS{}
	p := filepath.Join(t.TempDir(), "reference.jsonl")
	writeLines(t, p, lines)
	msgs, err := cfs.readTranscript(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// TestCodexIncrementalParse_MatchesFullParseAcrossAppends is the invariant the
// Codex reader's incremental cache rests on: growing a rollout one turn at a time
// must yield exactly what parsing the finished file from scratch yields — id
// pinning and usage badging included, both of which resolve a row written in an
// earlier chunk.
func TestCodexIncrementalParse_MatchesFullParseAcrossAppends(t *testing.T) {
	cfs := codexFS{}
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	var lines []string
	for n := 1; n <= 4; n++ {
		lines = append(lines, codexTurn(n)...)
		writeLines(t, path, lines)
		got, err := cfs.readTranscript(context.Background(), path)
		if err != nil {
			t.Fatalf("append %d: %v", n, err)
		}
		want := codexFullParse(t, lines)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("after append %d:\n got %+v\nwant %+v", n, got, want)
		}
		if len(got) != 2*n {
			t.Fatalf("after append %d: %d messages, want %d", n, len(got), 2*n)
		}
	}
}

// TestCodexIncrementalParse_SplitTurn is the case the mid-scan state exists for:
// the agent_message lands in one read and its id/usage lines only in the next, so
// a parse that rediscovered the pending row per chunk would leave the reply
// unbadged and id-less forever.
func TestCodexIncrementalParse_SplitTurn(t *testing.T) {
	cfs := codexFS{}
	path := filepath.Join(t.TempDir(), "split.jsonl")
	turn := codexTurn(1)
	writeLines(t, path, turn[:2]) // user + agent message only
	got, err := cfs.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "" || got[1].Usage != nil {
		t.Fatalf("baseline: want an unbadged, id-less reply, got %+v", got)
	}
	writeLines(t, path, turn) // the id and usage lines arrive
	got, err = cfs.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := codexFullParse(t, turn); !reflect.DeepEqual(got, want) {
		t.Fatalf("after the rest of the turn:\n got %+v\nwant %+v", got, want)
	}
	if got[1].ID != "msg_1" || got[1].Usage == nil {
		t.Fatalf("the pending reply did not pick up its id/usage across the append: %+v", got[1])
	}
}

// TestCodexIncrementalParse_AppendMidLine covers a rollout caught mid-write: a
// trailing partial line must be left unconsumed rather than parsed as garbage and
// skipped forever.
func TestCodexIncrementalParse_AppendMidLine(t *testing.T) {
	cfs := codexFS{}
	path := filepath.Join(t.TempDir(), "midwrite.jsonl")
	lines := codexTurn(1)
	writeLines(t, path, lines)
	if msgs, _ := cfs.readTranscript(context.Background(), path); len(msgs) != 2 {
		t.Fatalf("baseline: %d messages, want 2", len(msgs))
	}
	next := codexTurn(2)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(next[0]); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if msgs, _ := cfs.readTranscript(context.Background(), path); len(msgs) != 2 {
		t.Fatalf("mid-write: %d messages, want the partial line ignored (2)", len(msgs))
	}
	writeLines(t, path, append(lines, next...))
	got, err := cfs.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := codexFullParse(t, append(lines, next...)); !reflect.DeepEqual(got, want) {
		t.Fatalf("after completion:\n got %+v\nwant %+v", got, want)
	}
}

// TestCodexIncrementalParse_DetectsRewrite guards the append-only assumption: a
// rollout rewritten in place must fall back to a full re-parse rather than
// splicing new bytes onto a stale prefix.
func TestCodexIncrementalParse_DetectsRewrite(t *testing.T) {
	cfs := codexFS{}
	path := filepath.Join(t.TempDir(), "rewritten.jsonl")
	writeLines(t, path, codexTurn(1))
	if msgs, _ := cfs.readTranscript(context.Background(), path); len(msgs) != 2 {
		t.Fatalf("baseline: %d messages, want 2", len(msgs))
	}
	// Same path, different content, grown — the overlap check must reject it.
	rewritten := append(codexTurn(7), codexTurn(8)...)
	writeLines(t, path, rewritten)
	got, err := cfs.readTranscript(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if want := codexFullParse(t, rewritten); !reflect.DeepEqual(got, want) {
		t.Fatalf("after rewrite:\n got %+v\nwant %+v", got, want)
	}
}

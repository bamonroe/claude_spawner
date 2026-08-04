package session

import (
	"os"
	"path/filepath"
	"testing"
)

// A one-turn brain transcript.jsonl: a wrapped USER_INPUT, a null-content
// CONVERSATION_HISTORY + tool step (both ignored), and two PLANNER_RESPONSE steps
// (out of file order, to prove step sorting). The user content carries agy's
// <USER_REQUEST> envelope plus the metadata sections agy appends.
const agyBrainTranscript = `{"step_index":0,"type":"USER_INPUT","created_at":"2026-07-20T12:02:55Z","content":"<USER_REQUEST>\nRestart the container please\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-07-20T08:02:55-04:00.\n</ADDITIONAL_METADATA>"}
{"step_index":1,"type":"CONVERSATION_HISTORY","created_at":"2026-07-20T12:02:55Z","content":null}
{"step_index":2,"type":"RUN_COMMAND","created_at":"2026-07-20T12:02:56Z","content":"docker restart spawner"}
{"step_index":5,"type":"PLANNER_RESPONSE","created_at":"2026-07-20T12:03:01Z","content":"Done, the container is back up."}
{"step_index":3,"type":"PLANNER_RESPONSE","created_at":"2026-07-20T12:02:57Z","content":"On it — restarting now."}
`

const agyTestBrainID = "69eb04ae-a2d4-4b25-9b9e-71e4ebd3a77c"

// writeAgyBrain lays a transcript.jsonl at the real brain-dir layout under home so
// agyBrainDirFromPath recovers brainID from the path.
func writeAgyBrain(t *testing.T, home, brainID, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".gemini", "antigravity-cli", "brain", brainID, ".system_generated", "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAgyReadTranscript confirms a brain transcript replays as one unwrapped user row
// plus one joined assistant row (planner steps ordered by step_index, rejoined with
// blank lines), each carrying a durable brain-scoped id, tool/null steps ignored.
func TestAgyReadTranscript(t *testing.T) {
	home := t.TempDir()
	path := writeAgyBrain(t, home, agyTestBrainID, agyBrainTranscript)
	fs := antigravityFS{}
	msgs, err := fs.readTranscript(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	// User row: envelope unwrapped (our own scaffolding is stripped later by the
	// gateway, so it's absent here anyway), durable id, timestamp from created_at.
	if msgs[0].Role != "user" || msgs[0].Text != "Restart the container please" {
		t.Errorf("user row = %+v, want role=user text=%q", msgs[0], "Restart the container please")
	}
	if msgs[0].ID != agyTestBrainID+":in" {
		t.Errorf("user id = %q, want %q", msgs[0].ID, agyTestBrainID+":in")
	}
	if want := parseTs("2026-07-20T12:02:55Z"); msgs[0].Ts != want {
		t.Errorf("user Ts = %d, want %d", msgs[0].Ts, want)
	}
	// Assistant row: steps ordered 3 then 5 (despite file order), joined by blanks.
	wantReply := "On it — restarting now.\n\nDone, the container is back up."
	if msgs[1].Role != "claude" || msgs[1].Text != wantReply {
		t.Errorf("claude row = %+v, want role=claude text=%q", msgs[1], wantReply)
	}
	if msgs[1].ID != agyTestBrainID+":resp" {
		t.Errorf("claude id = %q, want %q", msgs[1].ID, agyTestBrainID+":resp")
	}
	if want := parseTs("2026-07-20T12:03:01Z"); msgs[1].Ts != want {
		t.Errorf("claude Ts = %d, want %d (the turn's last spoken step)", msgs[1].Ts, want)
	}
	if fs.lastContextUsage([]string{agyTestBrainID}) != nil {
		t.Error("agy records no token usage; want nil context snapshot")
	}
}

// TestAgyReadTranscriptChain_ReindexesAndKeepsDurableIDs confirms the AgyBrainIDs
// chain concatenates in turn order, contiguously re-indexed, while each row's durable
// id stays pinned to its brain — the property history reconciliation relies on.
func TestAgyReadTranscriptChain_ReindexesAndKeepsDurableIDs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const idA = "69eb04ae-a2d4-4b25-9b9e-71e4ebd3a77c"
	const idB = "60c2fae2-eb8f-4ed5-853f-05955e68fd15"
	writeAgyBrain(t, home, idA, agyBrainTranscript)
	writeAgyBrain(t, home, idB, agyBrainTranscript)
	fs := antigravityFS{}
	msgs, err := fs.readTranscriptChain([]string{idA, idB})
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []string{idA + ":in", idA + ":resp", idB + ":in", idB + ":resp"}
	if len(msgs) != len(wantIDs) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(wantIDs))
	}
	for i, w := range wantIDs {
		if msgs[i].Index != i {
			t.Errorf("msg %d Index = %d, want %d", i, msgs[i].Index, i)
		}
		if msgs[i].ID != w {
			t.Errorf("msg %d id = %q, want %q", i, msgs[i].ID, w)
		}
	}
}

// TestAgyFindByID confirms a brain id resolves to its deterministic transcript path,
// and an unsafe (non-UUID) id is refused rather than interpolated into a path.
func TestAgyFindByID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	fs := antigravityFS{}
	want := filepath.Join(home, ".gemini", "antigravity-cli", "brain", agyTestBrainID, agyTranscriptRel)
	if got := fs.findByID(agyTestBrainID); got != want {
		t.Errorf("findByID = %q, want %q", got, want)
	}
	if got := fs.findByID("../etc/passwd"); got != "" {
		t.Errorf("findByID(unsafe) = %q, want empty", got)
	}
	if got := fs.findByID(""); got != "" {
		t.Errorf("findByID(empty) = %q, want empty", got)
	}
}

// TestAgyUnwrapUserRequest covers the envelope extraction: inner text is returned
// trimmed, our appended scaffolding is preserved (the gateway strips it downstream),
// and a missing envelope falls back to the raw content unchanged.
func TestAgyUnwrapUserRequest(t *testing.T) {
	got := agyUnwrapUserRequest("<USER_REQUEST>\nhello world\n</USER_REQUEST>\n<ADDITIONAL_METADATA>x</ADDITIONAL_METADATA>")
	if got != "hello world" {
		t.Errorf("unwrap = %q, want %q", got, "hello world")
	}
	// Our own brief-reply scaffolding stays in place for the gateway's stripInjected.
	withScaffold := "<USER_REQUEST>\nrestart\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)\n</USER_REQUEST>"
	if got := agyUnwrapUserRequest(withScaffold); got != "restart\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)" {
		t.Errorf("unwrap kept scaffolding wrong: %q", got)
	}
	if got := agyUnwrapUserRequest("no envelope here"); got != "no envelope here" {
		t.Errorf("unwrap(no envelope) = %q, want unchanged", got)
	}
}

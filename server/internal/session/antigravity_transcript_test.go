package session

import (
	"os"
	"path/filepath"
	"testing"
)

// A brain-script output block for one turn: three PLANNER_RESPONSE messages (steps
// 142, 148, 151), interleaved with tool-only planner steps whose content is null,
// plus non-PLANNER lines that must be ignored. Mirrors the real transcript.jsonl
// record shape (type + step_index + content string|null).
const agyBlockThreeMsgs = agyMarker + `
{"step_index":140,"type":"VIEW_FILE","content":"...big file dump ignored..."}
{"step_index":142,"type":"PLANNER_RESPONSE","content":"I updated the app to support serving sizes."}
{"step_index":145,"type":"PLANNER_RESPONSE","content":null}
{"step_index":148,"type":"PLANNER_RESPONSE","content":"Ah, a couple of compile errors — fixed and rebuilding."}
{"step_index":151,"type":"PLANNER_RESPONSE","content":"The build finished. You should see the dropdown now!"}
`

const (
	agyMsg1 = "I updated the app to support serving sizes."
	agyMsg2 = "Ah, a couple of compile errors — fixed and rebuilding."
	agyMsg3 = "The build finished. You should see the dropdown now!"
)

func TestAgyPlannerMessages_OrdersAndFiltersByType(t *testing.T) {
	got := agyPlannerMessages(agyBlockThreeMsgs)
	want := []string{agyMsg1, agyMsg2, agyMsg3}
	if len(got) != len(want) {
		t.Fatalf("got %d messages, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("message %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAgyPlannerMessages_OrdersByStepIndexNotFileOrder(t *testing.T) {
	// Same messages presented out of step order: the reader must sort by step_index.
	block := agyMarker + "\n" +
		`{"step_index":151,"type":"PLANNER_RESPONSE","content":"third"}` + "\n" +
		`{"step_index":142,"type":"PLANNER_RESPONSE","content":"first"}` + "\n" +
		`{"step_index":148,"type":"PLANNER_RESPONSE","content":"second"}` + "\n"
	got := agyPlannerMessages(block)
	want := []string{"first", "second", "third"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestMatchAgyParagraphs_RebuildsParagraphsWhenSpaceJoinMatchesStdout(t *testing.T) {
	// agy's stdout blob is exactly the messages space-joined; the collapsed form is
	// what reconstructAgyReply passes as `want`.
	flat := agyMsg1 + " " + agyMsg2 + " " + agyMsg3
	para, _, ok := matchAgyParagraphs(agyBlockThreeMsgs, agyCollapseWS(flat))
	if !ok {
		t.Fatal("expected a match against the stdout blob")
	}
	want := agyMsg1 + "\n\n" + agyMsg2 + "\n\n" + agyMsg3
	if para != want {
		t.Errorf("reconstructed reply = %q, want %q", para, want)
	}
}

func TestMatchAgyParagraphs_PicksTheMatchingBlockAmongCandidates(t *testing.T) {
	// A newer, unrelated turn's transcript precedes ours in the output; only the
	// block whose content matches the stdout blob may be chosen.
	other := agyMarker + "\n" +
		`{"step_index":3,"type":"PLANNER_RESPONSE","content":"some other session reply"}` + "\n"
	out := other + agyBlockThreeMsgs
	flat := agyMsg1 + " " + agyMsg2 + " " + agyMsg3
	para, _, ok := matchAgyParagraphs(out, agyCollapseWS(flat))
	if !ok {
		t.Fatal("expected a match")
	}
	if want := agyMsg1 + "\n\n" + agyMsg2 + "\n\n" + agyMsg3; para != want {
		t.Errorf("got %q, want %q", para, want)
	}
}

func TestMatchAgyParagraphs_NoMatchFallsBack(t *testing.T) {
	// Nothing on disk reproduces the stdout blob (e.g. the transcript wasn't found or
	// agy changed its format) → no substitution, caller keeps the flat reply.
	if _, _, ok := matchAgyParagraphs(agyBlockThreeMsgs, agyCollapseWS("a completely different reply")); ok {
		t.Fatal("expected no match for an unrelated stdout blob")
	}
}

// The real brain-script emits "@@AGY@@ <path>" per block; matchAgyParagraphs must
// return the brain dir id parsed from that path so the caller can record which
// on-disk turn produced the reply (Session.AgyBrainIDs).
func TestMatchAgyParagraphs_CapturesBrainID(t *testing.T) {
	const id = "60c2fae2-eb8f-4ed5-853f-05955e68fd15"
	block := agyMarker + " /home/bam/.gemini/antigravity-cli/brain/" + id +
		"/.system_generated/logs/transcript.jsonl\n" +
		`{"step_index":142,"type":"PLANNER_RESPONSE","content":"` + agyMsg1 + `"}` + "\n"
	_, gotID, ok := matchAgyParagraphs(block, agyCollapseWS(agyMsg1))
	if !ok {
		t.Fatal("expected a match")
	}
	if gotID != id {
		t.Errorf("brain id = %q, want %q", gotID, id)
	}
}

// A block with no path payload (the synthetic marker the other tests use) yields no
// id rather than a bogus one — capture is best-effort and must degrade cleanly.
func TestAgyBrainID_NoPathYieldsEmpty(t *testing.T) {
	if got := agyBrainID(agyBlockThreeMsgs); got != "" {
		t.Errorf("brain id = %q, want empty for a pathless block", got)
	}
}

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

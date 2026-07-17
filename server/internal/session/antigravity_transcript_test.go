package session

import "testing"

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
	para, ok := matchAgyParagraphs(agyBlockThreeMsgs, agyCollapseWS(flat))
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
	para, ok := matchAgyParagraphs(out, agyCollapseWS(flat))
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
	if _, ok := matchAgyParagraphs(agyBlockThreeMsgs, agyCollapseWS("a completely different reply")); ok {
		t.Fatal("expected no match for an unrelated stdout blob")
	}
}

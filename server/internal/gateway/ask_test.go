package gateway

import "testing"

func TestParseAsk(t *testing.T) {
	// Clean block (possibly with prose around it — Claude sometimes adds a lead-in).
	reply := "I need a couple of things:\n::ASK::\n" +
		`[{"q":"Which auth?","options":["OAuth","API key"]},{"q":"Target dir?"}]` +
		"\n::END::\n"
	qs, ok := parseAsk(reply)
	if !ok || len(qs) != 2 {
		t.Fatalf("expected 2 questions, got ok=%v qs=%v", ok, qs)
	}
	if qs[0].Q != "Which auth?" || len(qs[0].Options) != 2 || qs[1].Options != nil {
		t.Fatalf("parsed questions wrong: %+v", qs)
	}

	// No block -> a normal answer.
	if _, ok := parseAsk("Sure, I'll do that now."); ok {
		t.Fatal("plain reply should not parse as ask")
	}
	// Malformed JSON -> fall back (not an ask).
	if _, ok := parseAsk("::ASK::\nnot json\n::END::"); ok {
		t.Fatal("malformed block should not parse as ask")
	}
	// Empty questions -> not an ask.
	if _, ok := parseAsk(`::ASK::[{"q":""}]::END::`); ok {
		t.Fatal("empty question should not parse as ask")
	}
}

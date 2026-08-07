package command

import "testing"

func TestParseSet(t *testing.T) {
	cases := []struct {
		in    string
		kind  Kind
		key   string
		value string
	}{
		{"set target bam", Set, "target", "bam"},
		{"set the target to bam", Set, "target", "bam"},
		{"set host to the workstation", Set, "target", "workstation"},
		{"set directory to slash data slash claude spawner", Set, "directory", "slash data slash claude spawner"},
		{"set folder /data/git", Set, "directory", "/data/git"},
		{"set target", Set, "target", ""}, // the gateway asks "set the target to what?"
		// Not settings: "set model" stays a model switch, anything outside the
		// closed key vocabulary falls through to dictation.
		{"set model 3", UseModel, "", ""},
		{"set the timer for five minutes", Unknown, "", ""},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.Kind != c.kind || (c.kind == Set && (got.Arg != c.key || got.Value != c.value)) {
			t.Errorf("Parse(%q) = %+v, want kind %q key %q value %q", c.in, got, c.kind, c.key, c.value)
		}
	}
}

func TestParseGet(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		key  string
	}{
		{"get target", Get, "target"},
		{"what is the target", Get, "target"},
		{"what's the directory", Get, "directory"},
		{"where is the directory", Get, "directory"},
		// A question that merely starts the same way is dictation, not a get.
		{"what is the target of this refactor", Unknown, ""},
		{"what is going on", Unknown, ""},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.Kind != c.kind || (c.kind == Get && got.Arg != c.key) {
			t.Errorf("Parse(%q) = %+v, want kind %q key %q", c.in, got, c.kind, c.key)
		}
	}
}

package command

import "testing"

func TestStripWake(t *testing.T) {
	cases := []struct {
		in      string
		rest    string
		hadWake bool
	}{
		{"hey buddy, spawn a new session", "spawn a new session", true},
		{"Hey Buddy spawn a session", "spawn a session", true},
		{"hey bud stop", "stop", true},
		{"Hey, bud, detach", "detach", true},
		{"hey body list sessions", "list sessions", true}, // whisper mishearing
		{"just some dictation", "just some dictation", false},
		{"hey there friend", "hey there friend", false},
	}
	for _, c := range cases {
		rest, had := StripWake(c.in)
		if rest != c.rest || had != c.hadWake {
			t.Errorf("StripWake(%q) = (%q,%v), want (%q,%v)", c.in, rest, had, c.rest, c.hadWake)
		}
	}
}

func TestSplitWake(t *testing.T) {
	cases := []struct {
		in     string
		before string
		after  string
		found  bool
	}{
		{"refactor the login handler", "refactor the login handler", "", false},
		{"fix the bug hey buddy detach", "fix the bug", "detach", true},
		{"hey buddy cancel message", "", "cancel message", true},
		{"add caching hey bud status", "add caching", "status", true},
		{"Hey, body, list sessions", "", "list sessions", true}, // mishearing, mid-strip
		// Multiple wakes: last command wins, dictation kept, middle discarded.
		{"fix the bug hey buddy detach hey buddy status", "fix the bug", "status", true},
		{"hey buddy list hey buddy detach", "", "detach", true},
	}
	for _, c := range cases {
		b, a, f := SplitWake(c.in)
		if b != c.before || a != c.after || f != c.found {
			t.Errorf("SplitWake(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, b, a, f, c.before, c.after, c.found)
		}
	}
}

func TestParseCancel(t *testing.T) {
	for _, in := range []string{"cancel", "cancel message", "cancel that", "scrap that", "never mind", "forget it"} {
		if got := Parse(in); got.Kind != Cancel {
			t.Errorf("Parse(%q).Kind = %s, want cancel", in, got.Kind)
		}
	}
}

func TestParseAbortTurn(t *testing.T) {
	// Abort must win over Cancel/Kill for these "…the turn" phrasings.
	for _, in := range []string{"abort", "abort the turn", "stop the turn", "stop the command",
		"cancel the turn", "kill the turn", "stop working"} {
		if got := Parse(in); got.Kind != AbortTurn {
			t.Errorf("Parse(%q).Kind = %s, want abort_turn", in, got.Kind)
		}
	}
	// A bare "cancel"/"kill" must NOT be read as abort.
	if Parse("cancel").Kind != Cancel {
		t.Errorf(`Parse("cancel") should stay Cancel`)
	}
}

func TestApplyAliases(t *testing.T) {
	al := map[string]string{"attached": "attach", "the tach": "detach", "listed": "list"}
	cases := []struct{ in, want string }{
		{"attached to claude-drat", "attach to claude-drat"},
		{"the tach", "detach"},
		{"listed sessions", "list sessions"},
		{"attach to foo", "attach to foo"}, // unchanged
		{"detach the auth module", "detach the auth module"},
	}
	for _, c := range cases {
		if got := ApplyAliases(c.in, al); got != c.want {
			t.Errorf("ApplyAliases(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// aliased mis-transcription then parses as the real command
	if got := Parse(ApplyAliases("attached to claude-drat", al)); got.Kind != Attach || got.Arg != "claude-drat" {
		t.Errorf("aliased attach = %+v", got)
	}
}

func TestParseHelp(t *testing.T) {
	for _, in := range []string{"help", "commands", "what can you do", "list commands", "which commands"} {
		if got := Parse(in); got.Kind != Help {
			t.Errorf("Parse(%q).Kind = %s, want help", in, got.Kind)
		}
	}
	if got := Parse("help me fix this bug"); got.Kind == Help {
		t.Errorf("Parse(%q) should not be help (dictation)", "help me fix this bug")
	}
}

func TestParseSpawn(t *testing.T) {
	cases := []struct {
		in       string
		new      bool
		location string
	}{
		{"spawn a new session", false, ""},
		{"spawn a session in git personal", false, "git personal"},
		{"spawn a new project in git personal", true, "git personal"},
		{"spawn a new project", true, ""},
		{"start a project under data", true, "data"},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.Kind != Spawn {
			t.Errorf("Parse(%q).Kind = %s, want spawn", c.in, got.Kind)
			continue
		}
		if got.New != c.new || got.Location != c.location {
			t.Errorf("Parse(%q) = {new:%v loc:%q}, want {new:%v loc:%q}",
				c.in, got.New, got.Location, c.new, c.location)
		}
	}
}

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		kind Kind
		arg  string
	}{
		{"spawn a new session", Spawn, ""},
		{"list sessions", List, ""},
		{"attach to claude-claude", Attach, "claude-claude"},
		{"detach", Detach, ""},
		{"kill session claude-claude", Kill, "claude-claude"},
		{"what's it doing", Status, ""},
		{"never mind", Cancel, ""},
		{"the weather is nice", Unknown, ""},
		// Dictation that must NOT be misread as a command (would flow to Claude).
		{"list the files in this directory", Unknown, ""},
		{"detach the auth module and refactor it", Unknown, ""},
		{"spawn a goroutine to handle requests", Unknown, ""},
		{"attach the debugger to the process", Unknown, ""},
		{"write a function that lists the sessions", Unknown, ""},
		// Short commands still work.
		{"list all", List, ""},
		{"detach", Detach, ""},
		// Whisper punctuation/capitalization must not break matching.
		{"List sessions.", List, ""},
		{"list sessions.", List, ""},
		{"Detach.", Detach, ""},
		{"kill session claude-claude.", Kill, "claude-claude"},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.Kind != c.kind || got.Arg != c.arg {
			t.Errorf("Parse(%q) = %+v, want {%s %q}", c.in, got, c.kind, c.arg)
		}
	}
}

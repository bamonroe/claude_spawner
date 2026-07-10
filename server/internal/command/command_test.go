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
		{"everybody detach", "detach", true},              // one-word collapse of "hey buddy"
		{"Everybody, status", "status", true},
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
		{"Hey, body, list sessions", "", "list sessions", true},         // mishearing, mid-strip
		{"fix the bug everybody detach", "fix the bug", "detach", true}, // one-word collapse mid-strip
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

func TestParseModels(t *testing.T) {
	for _, in := range []string{"list models", "what models", "which models", "show models", "list the models"} {
		if got := Parse(in); got.Kind != ListModels {
			t.Errorf("Parse(%q).Kind = %s, want list_models", in, got.Kind)
		}
	}
	// UseModel extracts the ordinal from either a digit or a number word.
	for in, want := range map[string]int{
		"use model 3":        3,
		"use model three":    3,
		"switch to model 2":  2,
		"select model one":   1,
		"use model number 4": 4,
	} {
		got := Parse(in)
		if got.Kind != UseModel || got.Count != want {
			t.Errorf("Parse(%q) = {%s, %d}, want {use_model, %d}", in, got.Kind, got.Count, want)
		}
	}
	// No number spoken → Count 0 (gateway reminds the user).
	if got := Parse("use model"); got.Kind != UseModel || got.Count != 0 {
		t.Errorf(`Parse("use model") = {%s, %d}, want {use_model, 0}`, got.Kind, got.Count)
	}
	// "list models" must not fall through to the bare List command.
	if Parse("list sessions").Kind != List {
		t.Error("list sessions should still be List")
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

func TestParseClear(t *testing.T) {
	for _, in := range []string{"clear", "clear context", "clear session", "clear the context",
		"reset context", "start fresh", "wipe context"} {
		if got := Parse(in); got.Kind != Clear {
			t.Errorf("Parse(%q).Kind = %s, want clear", in, got.Kind)
		}
	}
	// "clear history" must NOT match — clear keeps history for display, so this
	// stays dictation rather than implying deletion.
	if got := Parse("clear history"); got.Kind == Clear {
		t.Errorf(`Parse("clear history") = %s, must not be Clear`, got.Kind)
	}
}

func TestParseCompress(t *testing.T) {
	for _, in := range []string{"compress", "compress context", "compress session",
		"compact", "compact context", "compact the context", "condense context",
		"summarize the context", "compact it"} {
		if got := Parse(in); got.Kind != Compress {
			t.Errorf("Parse(%q).Kind = %s, want compress", in, got.Kind)
		}
	}
	// Dictation that merely mentions compressing/summarizing must NOT match — these
	// are longer, non-command utterances that should flow through to Claude.
	for _, in := range []string{"compress the image before uploading", "summarize the readme file"} {
		if got := Parse(in); got.Kind == Compress {
			t.Errorf("Parse(%q) = %s, must not be Compress", in, got.Kind)
		}
	}
}

func TestParseRename(t *testing.T) {
	// Each phrasing must parse to Rename and recover the intended new name.
	for _, tc := range []struct {
		in, want string
	}{
		{"rename to backend", "backend"},
		{"rename this session backend", "backend"},
		{"rename this to backend", "backend"},
		{"rename it backend", "backend"},
		{"call this backend", "backend"},
		{"call this session backend", "backend"},
		{"name this session backend", "backend"},
		{"rename backend", "backend"},
		{"rename this session to the api server", "the api server"},
	} {
		got := Parse(tc.in)
		if got.Kind != Rename {
			t.Errorf("Parse(%q).Kind = %s, want rename", tc.in, got.Kind)
			continue
		}
		if got.Arg != tc.want {
			t.Errorf("Parse(%q).Arg = %q, want %q", tc.in, got.Arg, tc.want)
		}
	}
	// Dictation that isn't a rename command must flow through to Claude, not be
	// hijacked. (Only wake-prefixed text reaches Parse, but the verb-led forms
	// still shouldn't over-match ordinary sentences.)
	for _, in := range []string{"rewrite this function", "the renamed field is stale"} {
		if got := Parse(in); got.Kind == Rename {
			t.Errorf("Parse(%q) = %s, must not be Rename", in, got.Kind)
		}
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
		// Inline location with no preposition: the path after "session"/"project"
		// is still captured so a one-shot command jumps straight there.
		{"spawn a new session bam git personal", false, "bam git personal"},
		{"new project data askii", true, "data askii"},
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

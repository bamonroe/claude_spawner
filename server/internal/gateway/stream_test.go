package gateway

import (
	"testing"

	"github.com/bam/claude_spawner/server/internal/command"
)

func TestGateDictation(t *testing.T) {
	speak := command.WakePhrase("take a note, dictate")
	cases := []struct {
		name string
		gate bool
		spk  [][]string
		in   string
		want string
	}{
		// Gate off: text passes through verbatim (current behavior).
		{"off", false, speak, "some ambient chatter", "some ambient chatter"},
		// Gate on, speak token present: only the bracketed remainder dictates.
		{"bracketed", true, speak, "radio noise take a note fix the bug", "fix the bug"},
		{"variant", true, speak, "blah dictate ship it", "ship it"},
		// Gate on, no speak token in the utterance: discard it all.
		{"chatter", true, speak, "just people talking nearby", ""},
		// Gate on but no speak token configured: fail safe — pass through, don't
		// silently swallow everything.
		{"no-token", true, nil, "still dictate this", "still dictate this"},
	}
	for _, c := range cases {
		cn := &conn{dictationGate: c.gate, speakPhrase: c.spk}
		if got := cn.gateDictation(c.in); got != c.want {
			t.Errorf("%s: gateDictation(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

func TestSplitEndToken(t *testing.T) {
	cases := []struct {
		text   string
		token  string
		before string
		after  string
		found  bool
	}{
		{"refactor this beep", "beep", "refactor this", "", true},
		{"refactor this", "beep", "refactor this", "", false},
		{"do it beep then more", "beep", "do it", "then more", true},
		{"stuff Beep.", "beep", "stuff", "", true},             // case + punctuation
		{"hello send it now", "send it", "hello", "now", true}, // multi-word token
		{"nothing here", "send it", "nothing here", "", false},
	}
	for _, c := range cases {
		b, a, f := splitEndToken(c.text, c.token)
		if b != c.before || a != c.after || f != c.found {
			t.Errorf("splitEndToken(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.text, c.token, b, a, f, c.before, c.after, c.found)
		}
	}
}

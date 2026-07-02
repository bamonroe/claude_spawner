package gateway

import "testing"

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

package fuzzy

import "testing"

func TestLevenshteinAndEqual(t *testing.T) {
	if d := Levenshtein("get", "git"); d != 1 {
		t.Errorf("Levenshtein(get,git) = %d, want 1", d)
	}
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"get", "git", true},
		{"personel", "personal", true},
		{"git", "data", false},
		{"cat", "dog", false},
	} {
		if got := Equal(c.a, c.b); got != c.want {
			t.Errorf("Equal(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPhraseDistanceIgnoresWordBoundaries(t *testing.T) {
	want := []string{"hey", "buddy"}
	for _, c := range []struct {
		name string
		said []string
		dist int
	}{
		{"exact", []string{"hey", "buddy"}, 0},
		{"collapsed into one word", []string{"heybuddy"}, 0},
		{"split differently", []string{"heybud", "dy"}, 0},
		{"mis-heard tail", []string{"hey", "buddha"}, 2},
		{"given as one spaced string", []string{"hey buddy"}, 0},
		{"unrelated", []string{"list", "the", "files"}, 11},
	} {
		if got := PhraseDistance(c.said, want); got != c.dist {
			t.Errorf("%s: PhraseDistance(%q, %q) = %d, want %d", c.name, c.said, want, got, c.dist)
		}
	}
}

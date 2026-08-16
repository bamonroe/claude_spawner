// Package fuzzy holds the repo's edit-distance helpers for comparing spoken
// words against canonical ones. It is deliberately dependency-free so any
// package can use it — the grammar leaf (command) as well as the filesystem
// side (projects, session) — without dragging one into the other.
package fuzzy

import "strings"

// Levenshtein returns the edit distance between a and b.
func Levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur := make([]int, lb+1)
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[lb]
}

// Equal reports whether two spoken tokens are close enough to be the same
// word, tolerating more slack as the words get longer.
func Equal(a, b string) bool {
	if a == b {
		return true
	}
	m := len(a)
	if len(b) < m {
		m = len(b)
	}
	d := Levenshtein(a, b)
	switch {
	case m <= 3:
		return d <= 1
	default:
		return d <= 2
	}
}

// PhraseDistance returns the edit distance between two multi-word phrases with
// their word boundaries removed, so a phrase Whisper split differently than the
// canonical one still scores on its letters alone: both "hey buddha" and a
// collapsed "heybuddy" measure against "hey buddy" as "heybuddha"/"heybuddy"
// versus "heybuddy". Words may be given as separate tokens or as one string
// containing spaces; both are joined the same way.
func PhraseDistance(a, b []string) int {
	return Levenshtein(joinPhrase(a), joinPhrase(b))
}

// joinPhrase concatenates words into one separator-free string.
func joinPhrase(words []string) string {
	var sb strings.Builder
	for _, w := range words {
		for _, r := range w {
			if r == ' ' || r == '\t' {
				continue
			}
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

package spoken

import "strings"

// Phrases returns the tokenized phrases ([][]string of lowercased,
// punctuation-trimmed words) of every token bound to the given action — the form
// the command package's matchers (StripWakeWith / SplitOn / …) accept. A token
// with a blank phrase is skipped. The result is the whole wake/end/gate set for
// that action: it REPLACES the old hardcoded "hey buddy" family rather than
// extending it (the built-ins survive only as the store's seed).
func Phrases(tokens []*Token, action string) [][]string {
	var out [][]string
	for _, t := range tokens {
		if t == nil || t.Action != action {
			continue
		}
		if p := tokenize(t.Phrase); len(p) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// Models returns the distinct, non-empty detector model keys of the tokens bound
// to the given action, to score against the sidecar's per-model scores. Empty
// when no token for the action carries a model (detector disabled for it).
func Models(tokens []*Token, action string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tokens {
		if t == nil || t.Action != action || t.Model == "" || seen[t.Model] {
			continue
		}
		seen[t.Model] = true
		out = append(out, t.Model)
	}
	return out
}

// DefaultTokens builds the first-run seed for the catalogue: one token per default
// wake phrase (the canonical first phrase carrying the wake detector model, the
// rest Whisper-only mishearing coverage) plus the default end token. Written once
// to the catalogue file; after that the app owns the list. Mirrors the behavior of
// the old hardcoded wake handling so a fresh deployment wakes on "hey buddy" and
// commits on "beep" out of the box.
func DefaultTokens(wakePhrases [][]string, wakeModel, endModel string) []*Token {
	out := make([]*Token, 0, len(wakePhrases)+1)
	for i, p := range wakePhrases {
		phrase := strings.Join(p, " ")
		t := &Token{Name: slug(phrase), Phrase: phrase, Action: ActionWake}
		if i == 0 { // the canonical wake phrase carries the detector model
			t.Model = wakeModel
		}
		out = append(out, t)
	}
	out = append(out, &Token{Name: "end-token", Phrase: "beep", Action: ActionEnd, Model: endModel})
	return out
}

// tokenize splits a phrase into lowercased, punctuation-trimmed words — the same
// normalization command's phrase matchers apply, so a token's phrase matches a
// transcript the identical way the built-in wake word did.
func tokenize(phrase string) []string {
	var out []string
	for _, w := range strings.Fields(strings.ToLower(phrase)) {
		if w = strings.Trim(w, ",.!?"); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// slug turns a phrase into a stable record name for seeding ("hey buddy" ->
// "hey-buddy"). Seed phrases are distinct, so the slugs are unique.
func slug(s string) string { return strings.ReplaceAll(strings.TrimSpace(s), " ", "-") }

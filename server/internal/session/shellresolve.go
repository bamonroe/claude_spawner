package session

import (
	"sort"
	"strings"

	"github.com/bam/claude_spawner/server/internal/projects"
)

// normalizeSpoken folds a spoken phrase to the form aliases are matched in:
// lowercase, sentence punctuation dropped, whitespace collapsed. It mirrors
// command.normalize so the shell catalogue and the "hey buddy" grammar agree on
// what two utterances being "the same words" means.
func normalizeSpoken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ',', '.', '!', '?', ';', ':', '"':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ResolveShellCommand matches the post-shell-token utterance against the
// catalogue and splits off its positional arguments.
//
// A record matches when its (normalized) Name is a leading *word* phrase of the
// (normalized) utterance — word-aligned, so "deployment" never matches an alias
// "deploy". When several records match, the one with the most alias words wins,
// so a "deploy staging" entry beats a plain "deploy" entry for "deploy staging
// now". Ties (equal word counts, only possible for distinct names differing in
// punctuation or case) resolve by lexical Name for determinism.
//
// The words after the matched alias are returned as the positional arguments,
// in order, for template expansion. ok is false when nothing matches.
func ResolveShellCommand(utterance string, catalogue []*ShellCommand) (*ShellCommand, []string, bool) {
	words := strings.Fields(normalizeSpoken(utterance))
	if len(words) == 0 {
		return nil, nil, false
	}
	var best *ShellCommand
	bestLen := 0
	for _, c := range catalogue {
		if c == nil {
			continue
		}
		alias := strings.Fields(normalizeSpoken(c.Name))
		if len(alias) == 0 || len(alias) > len(words) {
			continue
		}
		if len(alias) < bestLen {
			continue
		}
		match := true
		for i, w := range alias {
			if words[i] != w {
				match = false
				break
			}
		}
		if !match {
			continue
		}
		if len(alias) > bestLen || best == nil || c.Name < best.Name {
			best, bestLen = c, len(alias)
		}
	}
	if best == nil {
		return nil, nil, false
	}
	args := words[bestLen:]
	if len(args) == 0 {
		return best, nil, true
	}
	return best, append([]string(nil), args...), true
}

// SuggestShellCommands returns up to n catalogue names that sound closest to an
// utterance that matched nothing, so a spoken reply can offer a correction by
// ear ("did you mean disk space?"). Candidates are ranked by edit distance
// between the alias and the leading words of the utterance it would have had to
// match, and anything wildly different is dropped rather than guessed at.
func SuggestShellCommands(utterance string, catalogue []*ShellCommand, n int) []string {
	words := strings.Fields(normalizeSpoken(utterance))
	if len(words) == 0 || n <= 0 {
		return nil
	}
	type scored struct {
		name string
		d    int
	}
	var out []scored
	for _, c := range catalogue {
		if c == nil || strings.TrimSpace(c.Name) == "" {
			continue
		}
		alias := strings.Fields(normalizeSpoken(c.Name))
		if len(alias) == 0 {
			continue
		}
		// Compare the alias against the same number of leading spoken words, so a
		// two-word alias is judged on two words rather than the whole sentence.
		k := len(alias)
		if k > len(words) {
			k = len(words)
		}
		head := strings.Join(words[:k], " ")
		aliasNorm := strings.Join(alias, " ")
		d := projects.Levenshtein(head, aliasNorm)
		// Tolerate roughly a third of the alias being misheard, minimum one edit.
		limit := len(aliasNorm) / 3
		if limit < 1 {
			limit = 1
		}
		if d > limit {
			continue
		}
		out = append(out, scored{c.Name, d})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].d != out[j].d {
			return out[i].d < out[j].d
		}
		return out[i].name < out[j].name
	})
	if len(out) > n {
		out = out[:n]
	}
	names := make([]string, 0, len(out))
	for _, s := range out {
		names = append(names, s.name)
	}
	return names
}

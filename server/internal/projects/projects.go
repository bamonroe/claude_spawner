// Package projects indexes the directories under the configured roots and
// fuzzy-matches a spoken phrase against them, so the user can say "the spawner
// repo" instead of spelling an exact path. Used by the spawn dialog.
package projects

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Dir is a candidate project directory.
type Dir struct {
	Name string // basename
	Path string // absolute path
}

// Index holds the walkable directories under the roots.
type Index struct {
	roots    []string
	maxDepth int
	mu       sync.RWMutex
	dirs     []Dir
}

// New builds an Index over roots. maxDepth caps how deep the walk descends
// through namespace dirs to find repos (root -> namespace -> repo needs 3).
func New(roots []string) *Index {
	i := &Index{roots: roots, maxDepth: 3}
	i.Refresh()
	return i
}

// noise directories we never surface as projects.
var noise = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "dist": true,
	"build": true, "__pycache__": true, ".venv": true, "venv": true,
}

// Refresh re-walks the roots. A "project" is a git repository; the walk adds
// each repo as a leaf (never descending into it) and descends through non-repo
// "namespace" dirs (those that contain repos, e.g. ~/git/SparkyFitness) to reach
// them. Plain top-level dirs with no repos inside (e.g. /data/jellyfin) are
// listed too, so service folders stay reachable. Results are sorted by name.
func (i *Index) Refresh() {
	var dirs []Dir
	seen := map[string]bool{}
	add := func(d Dir) {
		if !seen[d.Path] {
			dirs = append(dirs, d)
			seen[d.Path] = true
		}
	}
	for _, root := range i.roots {
		i.walk(root, 0, add)
	}
	sort.SliceStable(dirs, func(a, b int) bool {
		return strings.ToLower(dirs[a].Name) < strings.ToLower(dirs[b].Name)
	})
	i.mu.Lock()
	i.dirs = dirs
	i.mu.Unlock()
}

func (i *Index) walk(dir string, depth int, add func(Dir)) {
	for _, name := range subdirs(dir) {
		path := filepath.Join(dir, name)
		switch {
		case isRepo(path):
			add(Dir{Name: name, Path: path}) // a project; never descend into it
		case anyRepoChild(path):
			if depth+1 < i.maxDepth { // a namespace; descend for its repos
				i.walk(path, depth+1, add)
			}
		case depth == 0:
			add(Dir{Name: name, Path: path}) // top-level plain dir (service folder)
		}
	}
}

// subdirs returns the non-hidden, non-noise subdirectory names of dir.
func subdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || noise[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// isRepo reports whether dir is a git repository (worktrees have a .git file).
func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// anyRepoChild reports whether any immediate subdirectory of dir is a repo,
// i.e. dir is a namespace/container of projects.
func anyRepoChild(dir string) bool {
	for _, name := range subdirs(dir) {
		if isRepo(filepath.Join(dir, name)) {
			return true
		}
	}
	return false
}

// List returns up to n projects (name-deduped, already sorted), for "list
// projects".
func (i *Index) List(n int) []Dir {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]Dir, 0, n)
	seen := map[string]bool{}
	for _, d := range i.dirs {
		if len(out) >= n {
			break
		}
		if seen[d.Name] {
			continue
		}
		seen[d.Name] = true
		out = append(out, d)
	}
	return out
}

// filler words dropped from a spoken directory query.
var filler = map[string]bool{
	"the": true, "a": true, "my": true, "repo": true, "repository": true,
	"project": true, "folder": true, "dir": true, "directory": true,
	"session": true, "code": true, "in": true, "into": true, "to": true,
	"for": true, "one": true, "called": true, "named": true,
}

// Children returns the immediate (non-hidden, non-noise) subdirectories of dir,
// case-insensitively sorted by name.
func Children(dir string) []Dir {
	var out []Dir
	for _, name := range subdirs(dir) {
		out = append(out, Dir{Name: name, Path: filepath.Join(dir, name)})
	}
	sort.SliceStable(out, func(a, b int) bool {
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out
}

// IsNamespace reports whether dir is a container of projects (not itself a repo,
// but has repo children) — e.g. ~/git or ~/git/SparkyFitness. Such dirs should
// be descended into rather than used as a session directory.
func IsNamespace(dir string) bool {
	return !isRepo(dir) && anyRepoChild(dir)
}

// IsRepo reports whether dir is a git repository.
func IsRepo(dir string) bool { return isRepo(dir) }

// Terms tokenizes a spoken query and drops filler words.
func Terms(query string) []string {
	var terms []string
	for _, t := range tokenize(query) {
		if !filler[t] {
			terms = append(terms, t)
		}
	}
	return terms
}

// Rank scores a set of directories against the spoken query, best first, keeping
// only near-top-score matches (so a clear winner returns alone).
func Rank(query string, dirs []Dir) []Dir {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil
	}
	qJoined := strings.Join(terms, "")

	type scored struct {
		Dir
		score int
	}
	var results []scored
	for _, d := range dirs {
		nameTokens := tokenize(d.Name)
		nameJoined := strings.Join(nameTokens, "")
		score := 0
		switch {
		case nameJoined == qJoined:
			score = 1000 // exact (ignoring separators)
		case strings.Contains(nameJoined, qJoined):
			score = 500 // query is a contiguous chunk of the name
		}
		// Per-term substring matches against the name.
		matched := 0
		for _, t := range terms {
			if strings.Contains(nameJoined, t) {
				matched++
				score += 40
				continue
			}
			// Tolerate transcription slips ("get" for "git", "personel" for
			// "personal") via edit distance against the name's tokens.
			for _, nt := range nameTokens {
				if FuzzyEqual(t, nt) {
					matched++
					score += 25
					break
				}
			}
		}
		if matched == len(terms) {
			score += 60 // all terms present
		}
		if score == 0 {
			continue
		}
		// Prefer shorter names (less noise) as a tiebreak.
		score -= len(nameJoined)
		results = append(results, scored{d, score})
	}
	if len(results) == 0 {
		return nil
	}
	sort.SliceStable(results, func(a, b int) bool { return results[a].score > results[b].score })

	// Keep only results near the top score, so a clear winner (e.g. an exact
	// match) is returned alone rather than buried among weak short-token hits
	// like "pi" matching "gitea_api".
	cutoff := results[0].score * 6 / 10

	// Dedupe by name so the pick list never shows "jellyfin, jellyfin"; the
	// highest-scored path for a given name wins.
	out := make([]Dir, 0, len(results))
	seenName := map[string]bool{}
	for _, r := range results {
		if r.score < cutoff || seenName[r.Name] {
			continue
		}
		seenName[r.Name] = true
		out = append(out, r.Dir)
	}
	return out
}

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

// FuzzyEqual reports whether two spoken tokens are close enough to be the same
// word, tolerating more slack as the words get longer.
func FuzzyEqual(a, b string) bool {
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

// tokenize lowercases and splits on non-alphanumeric plus camelCase boundaries.
func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	runes := []rune(s)
	for idx, r := range runes {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum {
			flush()
			continue
		}
		// camelCase boundary: lower/digit followed by upper.
		if idx > 0 && r >= 'A' && r <= 'Z' {
			prev := runes[idx-1]
			if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
				flush()
			}
		}
		cur.WriteRune(r)
	}
	flush()
	return tokens
}

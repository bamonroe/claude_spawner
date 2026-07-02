// Package pathspeak normalizes a spoken directory path into a real filesystem
// path. Speech is inherently ambiguous, so this is a best-effort heuristic
// documented in docs/commands.md.
//
// Rules:
//   - Each spoken word is a path segment, joined by "/".
//   - Connector words merge the surrounding segments without a "/":
//     "underscore" -> "_", "dash"/"hyphen" -> "-".
//   - "dot"/"period" starts a new dotted segment (".config", ".claude").
//   - "slash" is an explicit segment boundary (and marks an absolute path if first).
//   - The result is always absolute (leading "/"). The caller is responsible for
//     jailing it against an allowed root (see config.ValidateSpawnDir).
//
// Example: "data claude underscore claude" -> "/data/claude_claude".
package pathspeak

import (
	"fmt"
	"strings"
)

// connectors join the surrounding segments in-place (no "/").
var connectors = map[string]string{
	"underscore": "_",
	"dash":       "-",
	"hyphen":     "-",
}

// Normalize converts a spoken phrase into an absolute path.
func Normalize(spoken string) (string, error) {
	words := strings.Fields(strings.ToLower(strings.TrimSpace(spoken)))
	if len(words) == 0 {
		return "", fmt.Errorf("empty path")
	}

	var segments []string
	mergeNext := false
	for i, w := range words {
		if w == "slash" {
			if i == 0 {
				continue // leading slash just means absolute, which we force anyway
			}
			mergeNext = false
			continue
		}
		// "dot"/"period" begins a new dotted segment (e.g. ".config", ".claude"),
		// then merges the following word onto it.
		if w == "dot" || w == "period" {
			segments = append(segments, ".")
			mergeNext = true
			continue
		}
		if c, ok := connectors[w]; ok {
			if len(segments) == 0 {
				segments = append(segments, "")
			}
			segments[len(segments)-1] += c
			mergeNext = true
			continue
		}
		if mergeNext && len(segments) > 0 {
			segments[len(segments)-1] += w
			mergeNext = false
		} else {
			segments = append(segments, w)
		}
	}

	joined := strings.Join(segments, "/")
	if joined == "" {
		return "", fmt.Errorf("no path segments in %q", spoken)
	}
	return "/" + strings.TrimLeft(joined, "/"), nil
}

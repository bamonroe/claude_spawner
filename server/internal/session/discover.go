package session

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discovered is a Claude session found on disk (via its transcript) that isn't
// necessarily in the spawner's registry — used to surface sessions started
// outside the spawner (e.g. interactive `claude` in tmux) so they can be adopted.
type Discovered struct {
	SessionID  string
	Dir        string
	LastActive int64 // transcript mtime, unix seconds
}

// DiscoverSessions scans ~/.claude/projects for every Claude session transcript
// and returns each session's id + working directory + last-active time, newest
// first. Transcripts whose working directory can't be recovered are skipped.
func DiscoverSessions() ([]Discovered, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", "*.jsonl"))
	seen := map[string]bool{}
	out := make([]Discovered, 0, len(matches))
	for _, p := range matches {
		id := strings.TrimSuffix(filepath.Base(p), ".jsonl")
		if !looksLikeUUID(id) || seen[id] {
			continue
		}
		dir := transcriptCwd(p)
		if dir == "" {
			continue
		}
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		seen[id] = true
		out = append(out, Discovered{SessionID: id, Dir: dir, LastActive: info.ModTime().Unix()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastActive > out[j].LastActive })
	// One entry per directory: keep the most-recently-active session (the one
	// `claude --resume` would continue), not every historical session in that dir.
	byDir := map[string]bool{}
	deduped := out[:0]
	for _, d := range out {
		if byDir[d.Dir] {
			continue
		}
		byDir[d.Dir] = true
		deduped = append(deduped, d)
	}
	return deduped, nil
}

// transcriptCwd returns the first `cwd` recorded in a transcript (present on
// most events). Reads only the head of the file, not the whole thing.
func transcriptCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for i := 0; i < 40 && sc.Scan(); i++ {
		var ev struct {
			Cwd string `json:"cwd"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) == nil && ev.Cwd != "" {
			return ev.Cwd
		}
	}
	return ""
}

// looksLikeUUID reports whether s is an 8-4-4-4-12 hex UUID (a Claude session id).
func looksLikeUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return false
			}
		}
	}
	return true
}

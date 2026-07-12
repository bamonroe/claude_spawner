package session

import (
	"encoding/json"
)

// Discovered is a Claude session found on disk (via its transcript) that isn't
// necessarily in the spawner's registry — used to surface sessions started
// outside the spawner (e.g. interactive `claude` in tmux) so they can be adopted.
type Discovered struct {
	SessionID  string
	Dir        string
	LastActive int64 // transcript mtime, unix seconds
}

// DiscoverSessions scans the LOCAL ~/.claude/projects for every Claude session
// transcript and returns each session's id + working directory + last-active time,
// newest first. Transcripts whose working directory can't be recovered are skipped.
// For a specific host (local or remote), go through Driver.claudeFSFor.
func DiscoverSessions() ([]Discovered, error) {
	return localClaudeFS.discoverSessions()
}

// DiscoverSessions scans a host's ~/.claude/projects for every Claude session
// transcript (empty host = the loopback machine over SSH when SSH-native is wired).
// It's how sessions started outside the spawner are surfaced for adoption.
func (d *Driver) DiscoverSessions(host string) ([]Discovered, error) {
	return d.claudeFSFor(host).discoverSessions()
}

// transcriptCwd returns the first `cwd` recorded in a transcript (present on most
// events), reading only the head of the file. Backend-neutral: local or over SSH.
func (fs claudeFS) transcriptCwd(path string) string {
	f, err := fs.open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := newLineScanner(f)
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

// TranscriptCwd reads the working directory from a LOCAL transcript.
func TranscriptCwd(path string) string { return localClaudeFS.transcriptCwd(path) }

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

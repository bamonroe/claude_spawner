// Package tmux inspects tmux to detect Claude Code sessions a human is running
// interactively in a terminal.
//
// The voice data path does NOT use tmux — it drives Claude Code headless via the
// session package (stream-json). This package exists only so the spawner can
// notice when a directory already has an interactive `claude` open in a pane and
// warn before driving that same on-disk session_id headlessly (two writers on
// one session conflict).
package tmux

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// dirsTTL is how long one `tmux list-panes` result is reused. A single
// broadcast fans a discover out to every connected device, and each one asks
// for the pane list; without this, one clear forks tmux once per device. Panes
// don't change meaningfully inside a second, so a short memo collapses the
// storm to one fork while staying effectively live.
const dirsTTL = time.Second

// Manager inspects tmux panes.
type Manager struct {
	// Bin is the tmux binary (default "tmux").
	Bin string

	mu     sync.Mutex
	cached map[string]bool // last ClaudeDirs result
	at     time.Time       // when it was taken; zero = none
	now    func() time.Time
}

// NewManager returns a Manager with project defaults.
func NewManager() *Manager {
	return &Manager{Bin: "tmux"}
}

// ClaudeDirs returns the set of working directories that currently have an
// interactive `claude` running in a tmux pane. Used to warn before the spawner
// drives a session headlessly — two writers on the same session conflict.
// Best-effort: returns empty on any error.
//
// The result is memoized for dirsTTL, and the fork runs under the memo lock so
// a burst of concurrent callers (the per-device discover fan-out) collapses to
// a single `tmux list-panes` rather than one per caller.
func (m *Manager) ClaudeDirs(ctx context.Context) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now
	if m.now != nil {
		now = m.now
	}
	if m.cached != nil && now().Sub(m.at) < dirsTTL {
		return copyDirs(m.cached)
	}
	dirs := map[string]bool{}
	cmd := exec.CommandContext(ctx, m.Bin, "list-panes", "-a", "-F", "#{pane_current_command}\t#{pane_current_path}")
	out, err := cmd.Output()
	if err != nil {
		return dirs
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) == 2 && strings.Contains(parts[0], "claude") {
			dirs[parts[1]] = true
		}
	}
	m.cached, m.at = dirs, now()
	return copyDirs(dirs)
}

// copyDirs hands each caller its own map, so the memoized one is never mutated
// under another caller's feet.
func copyDirs(src map[string]bool) map[string]bool {
	out := make(map[string]bool, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

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
)

// Manager inspects tmux panes.
type Manager struct {
	// Bin is the tmux binary (default "tmux").
	Bin string
}

// NewManager returns a Manager with project defaults.
func NewManager() *Manager {
	return &Manager{Bin: "tmux"}
}

// ClaudeDirs returns the set of working directories that currently have an
// interactive `claude` running in a tmux pane. Used to warn before the spawner
// drives a session headlessly — two writers on the same session conflict.
// Best-effort: returns empty on any error.
func (m *Manager) ClaudeDirs(ctx context.Context) map[string]bool {
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
	return dirs
}

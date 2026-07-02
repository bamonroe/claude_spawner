// Package tmux provides the OPTIONAL human-babysit view of a session.
//
// The voice data path does NOT use tmux — it drives Claude Code headless via the
// session package (stream-json). tmux exists only so a human can `tmux attach`
// from a real terminal and watch/take over the interactive TUI for the SAME
// on-disk session_id. Treat a session as having one active writer at a time:
// don't run a headless turn and an interactive babysit pane against the same
// session_id simultaneously.
package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Manager spawns and controls tmux panes that host interactive Claude Code TUIs.
type Manager struct {
	// Bin is the tmux binary (default "tmux").
	Bin string
	// ClaudeBin is the claude binary launched inside the pane (default "claude").
	ClaudeBin string
	// Bypass adds --dangerously-skip-permissions to the babysit TUI when true.
	Bypass bool
}

// NewManager returns a Manager with project defaults.
func NewManager() *Manager {
	return &Manager{Bin: "tmux", ClaudeBin: "claude", Bypass: true}
}

// Babysit opens a detached tmux session named `name` in `dir` running an
// interactive `claude --resume <sessionID>` TUI, so a human can `tmux attach
// -t <name>` to watch or take over. sessionID must be an existing on-disk
// Claude session (created by the session package's first headless turn).
func (m *Manager) Babysit(ctx context.Context, name, dir, sessionID string) error {
	inner := fmt.Sprintf("%s --resume %s", m.ClaudeBin, sessionID)
	if m.Bypass {
		inner += " --dangerously-skip-permissions"
	}
	cmd := exec.CommandContext(ctx, m.Bin,
		"new-session", "-d", "-s", name, "-c", dir, inner)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("babysit %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// List returns the names of currently-open babysit panes.
func (m *Manager) List(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, m.Bin, "list-sessions", "-F", "#{session_name}")
	out, err := cmd.Output()
	if err != nil {
		// tmux exits 1 with "no server running" when there are no sessions.
		if strings.Contains(err.Error(), "exit status 1") {
			return nil, nil
		}
		return nil, fmt.Errorf("list-sessions: %w", err)
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// ClaudeDirs returns the set of working directories that currently have an
// interactive `claude` running in a tmux pane (any session, not just babysit
// panes). Used to warn before the spawner drives a session headlessly — two
// writers on the same session conflict. Best-effort: returns empty on any error.
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

// Exists reports whether a babysit pane with the given name is open.
func (m *Manager) Exists(ctx context.Context, name string) (bool, error) {
	names, err := m.List(ctx)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}

// Close terminates a babysit pane (does not affect the on-disk session_id).
func (m *Manager) Close(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, m.Bin, "kill-session", "-t", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("close babysit %q: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

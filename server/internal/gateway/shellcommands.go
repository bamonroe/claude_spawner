package gateway

import (
	"errors"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The shell-command catalogue (Settings → Shell commands) is app-managed exactly
// like the spoken tokens: the app is the source of truth, the server persists it
// (session.ShellCommandStore) so it survives restarts and is shared across
// clients. These handlers service the put/delete wire messages and broadcast the
// updated catalogue to every client on a change. The list is pushed on connect
// (msgShellCommands) behind the shell_commands_digest fast path, so there is no
// separate list-request message.

// broadcastShellCommands re-sends the full catalogue to every connected client.
func (c *conn) broadcastShellCommands() {
	c.srv.broadcast(msgShellCommands(c.srv.shellCmds.List()))
}

// doShellCommandPut adds or updates a shell command (upsert by name) and
// broadcasts. Both a name and a command template are required — a nameless entry
// is unspeakable and an empty template is unrunnable.
func (c *conn) doShellCommandPut(cmd *session.ShellCommand) {
	if cmd == nil || cmd.Name == "" {
		c.fail("bad_shell_command", "shell command needs a name")
		return
	}
	if err := c.srv.shellCmds.Put(cmd); err != nil {
		if errors.Is(err, session.ErrStale) {
			// A newer edit already won: re-send our catalogue so the stale client adopts it.
			c.send(msgShellCommands(c.srv.shellCmds.List()))
			return
		}
		c.fail("bad_shell_command", err.Error())
		return
	}
	c.broadcastShellCommands()
}

// doShellCommandDelete removes a shell command by name (tombstoning it at
// updatedAt) and broadcasts. A delete older than the stored record re-syncs the
// stale client instead of erroring.
func (c *conn) doShellCommandDelete(name string, updatedAt int64) {
	if name == "" {
		c.fail("bad_shell_command", "need a shell command name to delete")
		return
	}
	if err := c.srv.shellCmds.Delete(name, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgShellCommands(c.srv.shellCmds.List()))
			return
		}
		c.fail("internal", err.Error())
		return
	}
	c.broadcastShellCommands()
}

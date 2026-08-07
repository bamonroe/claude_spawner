package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// runShell handles an utterance that came in through the shell token: the text is
// the alias of a PRE-CONFIGURED shell command plus its spoken arguments. Arbitrary
// spoken shell is never run — the catalogue of aliases is the safety boundary, which
// is why there is no confirmation prompt for destructive-looking commands.
//
// Everything this function says is read aloud, so each reply is one short spoken
// sentence and every dead end tells the user what to say next: an unknown alias
// offers the closest configured names so they can correct by ear, and a template
// whose $1..$9 went unfilled asks for the argument rather than running with a
// blank in its place.
//
// A fully resolved command runs on the configured target host, in the configured
// directory, over the shared SSH pool — see execShell.
func (c *conn) runShell(text string) {
	if strings.TrimSpace(text) == "" {
		c.send(msgSay("shell what?"))
		return
	}
	catalogue := c.srv.shellCmds.List()
	if len(catalogue) == 0 {
		c.send(msgSay("no shell commands are configured yet."))
		return
	}
	cmd, args, ok := session.ResolveShellCommand(text, catalogue)
	if !ok {
		c.send(msgSay(unknownShellReply(text, catalogue)))
		return
	}
	if missing := session.MissingShellArgs(cmd.Command, args); len(missing) > 0 {
		c.send(msgSay(missingArgReply(cmd.Name, len(missing))))
		return
	}
	c.execShell(cmd, args)
}

// shellRunTimeout bounds one spoken shell command. The catalogue holds short
// operational commands, not long jobs — a command that hasn't finished by now is
// reported as timed out rather than left holding a pooled SSH channel forever.
const shellRunTimeout = 60 * time.Second

// execShell expands the template with the spoken arguments and runs it on the
// target host over the shared SSH pool, in the configured directory. It runs off
// the connection's read loop because a command takes as long as it takes, and the
// whole of its output — stdout and stderr merged, the way a terminal shows it — is
// spoken back.
func (c *conn) execShell(cmd *session.ShellCommand, args []string) {
	line := session.ExpandShellCommand(cmd.Command, args)
	// A catalogue entry may pin its own directory and host; otherwise the shared
	// "set directory" / "set target" settings decide where it runs.
	dir := strings.TrimSpace(cmd.Dir)
	if dir == "" {
		dir = c.srv.targetDir()
	}
	if dir != "" {
		line = "cd " + shellQuoteArg(dir) + " && " + line
	}
	// Merge stderr into stdout: the pool returns stdout only, and a command's
	// complaint is exactly what the user needs read aloud when it fails.
	line = "{ " + line + " ; } 2>&1"
	host := strings.TrimSpace(cmd.Host)
	if host == "" {
		host = c.srv.targetHost()
	}
	if c.srv.ssh == nil {
		c.send(msgSay("shell commands need ssh, which is turned off."))
		return
	}
	c.send(msgSay(fmt.Sprintf("running %s on %s.", cmd.Name, host)))
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, shellRunTimeout)
		defer cancel()
		out, err := c.srv.ssh.Run(ctx, host, line)
		c.send(msgSay(shellOutputReply(cmd.Name, out, err, ctx.Err() != nil)))
	}()
}

// shellOutputReply turns a finished command into one spoken reply: its full output
// when it produced any, and otherwise a sentence saying what happened, so silence
// on the phone never means "did that even run?".
func shellOutputReply(name string, out []byte, err error, timedOut bool) string {
	text := strings.TrimSpace(string(out))
	switch {
	case timedOut:
		if text == "" {
			return fmt.Sprintf("%s timed out.", name)
		}
		return fmt.Sprintf("%s timed out. it said: %s", name, text)
	case err != nil:
		if text == "" {
			return fmt.Sprintf("%s failed: %s", name, err)
		}
		return fmt.Sprintf("%s failed. it said: %s", name, text)
	case text == "":
		return fmt.Sprintf("%s finished with no output.", name)
	default:
		return text
	}
}

// unknownShellReply names the alias we could not find and, when something in the
// catalogue is close, offers one or two alternatives to say instead.
func unknownShellReply(text string, catalogue []*session.ShellCommand) string {
	spoken := strings.Join(strings.Fields(text), " ")
	base := fmt.Sprintf("i do not know a shell command called %s.", spoken)
	switch near := session.SuggestShellCommands(text, catalogue, 2); len(near) {
	case 0:
		return base
	case 1:
		return base + fmt.Sprintf(" did you mean %s?", near[0])
	default:
		return base + fmt.Sprintf(" did you mean %s or %s?", near[0], near[1])
	}
}

// missingArgReply asks for the arguments a template needs but the speaker left
// out, and shows the shape to repeat.
func missingArgReply(name string, n int) string {
	if n == 1 {
		return fmt.Sprintf("%s needs an argument. say %s and then the value.", name, name)
	}
	return fmt.Sprintf("%s needs %d arguments. say %s and then all %d values.", name, n, name, n)
}

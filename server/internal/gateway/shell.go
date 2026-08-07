package gateway

import (
	"fmt"
	"strings"

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
// The SSH execution itself lands in the follow-up task of the shell-token epic
// (see TODO.toml); until then a fully resolved command says so.
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
	c.send(msgSay(fmt.Sprintf("%s is not wired to run yet.", cmd.Name)))
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

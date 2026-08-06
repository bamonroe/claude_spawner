package gateway

import "strings"

// runShell handles an utterance that came in through the shell token: the text is
// the alias of a PRE-CONFIGURED shell command plus its spoken arguments. Arbitrary
// spoken shell is never run — the catalogue of aliases is the safety boundary, which
// is why there is no confirmation prompt for destructive-looking commands.
//
// The catalogue, the alias/argument parsing and the SSH execution land in the
// follow-up tasks of the shell-token epic (see TODO.toml); until then the gate is
// wired end to end but has nothing to resolve against, and says so.
func (c *conn) runShell(text string) {
	if strings.TrimSpace(text) == "" {
		c.send(msgSay("shell what?"))
		return
	}
	c.send(msgSay("no shell commands are configured yet."))
}

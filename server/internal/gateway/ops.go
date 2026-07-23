package gateway

import (
	"github.com/bam/claude_spawner/server/internal/command"
)

// dispatch handles an immediate (push-to-talk / typed) utterance when no dialog
// is active: a control command, or dictation to the attached session.
func (c *conn) dispatch(text string) {
	rest, hadWake := c.stripWake(text)

	// Attached + no wake word => plain dictation.
	if c.attached != nil && !hadWake {
		c.dictate(text)
		return
	}

	if c.runCommand(command.Parse(command.ApplyAliases(rest, c.aliases))) {
		return
	}
	// Wake word present but not a command: dictate it (wake-stripped) when
	// attached, else nudge.
	if c.attached != nil {
		c.dictate(rest)
	} else if c.scratch {
		c.send(msgSay(rest)) // scratch mode: read back exactly what was transcribed
	} else {
		c.send(msgSay("didn't catch that. try 'spawn a new session'."))
	}
}

// runCommand executes a recognized control command, returning false if the
// intent is Unknown (so the caller decides what to do). Shared by the immediate
// path (dispatch) and the hands-free commit path (commitMessage).

// runCommand executes a recognized control command, returning false if the
// intent is Unknown (so the caller decides what to do). Shared by the immediate
// path (dispatch) and the hands-free commit path (commitMessage).
func (c *conn) runCommand(intent command.Intent) bool {
	switch intent.Kind {
	case command.Spawn:
		c.spawnCommand(intent)
	case command.List:
		c.doList()
	case command.Attach:
		c.doAttach(intent.Arg, false) // voice attach announces
	case command.Detach:
		c.doDetach()
	case command.Swap:
		c.doSwap()
	case command.Kill:
		c.doKill(intent.Arg)
	case command.Status:
		c.doStatus()
	case command.Stop:
		c.send(msgStopSpeaking())
	case command.AbortTurn:
		c.abortTurn()
	case command.Cancel:
		c.send(msgSay("nothing to cancel."))
	case command.Help:
		c.send(msgSay(commandHelp))
	case command.ReadLast:
		c.send(msgReadLast(intent.Count))
	case command.Clear:
		c.doClear()
	case command.Compress:
		c.doCompress()
	case command.Usage:
		c.doUsage(true) // voice command: show the report AND speak a summary
	case command.Rename:
		c.doRenameCurrent(intent.Arg)
	case command.ListModels:
		c.doListModels()
	case command.UseModel:
		c.doUseModel(intent.Count)
	case command.Scratch:
		c.doScratch(intent.Arg)
	case command.SummaryOnly:
		c.doSummaryOnly(intent.Arg)
	case command.ListJobs:
		c.doListJobs()
	case command.KillJob:
		c.doKillJob(intent.Count)
	case command.JobStatus:
		c.doJobStatus()
	case command.Restart:
		c.doRestart("") // voice "restart" = full rebuild (empty = rebuild)
	default:
		return false
	}
	return true
}

// doScratch toggles scratch mode. Arg "on"/"off" sets it explicitly; "" flips
// it. Scratch mode only echoes while detached (see dispatch/commitMessage), so
// it never interferes with an attached session.

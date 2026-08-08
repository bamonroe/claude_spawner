package gateway

import (
	"context"
	"strconv"
)

// doScratch toggles scratch mode. Arg "on"/"off" sets it explicitly; "" flips
// it. Scratch mode only echoes while detached (see dispatch/commitMessage), so
// it never interferes with an attached session.
func (c *conn) doScratch(arg string) {
	switch arg {
	case "on":
		c.scratch = true
	case "off":
		c.scratch = false
	default:
		c.scratch = !c.scratch
	}
	if c.scratch {
		c.send(msgSay("scratch mode on — detach and speak, and I'll read back what I heard. say 'scratch off' to stop."))
	} else {
		c.send(msgSay("scratch mode off."))
	}
}

// doSummaryOnly toggles summary-only speech (the voice "summary only" / "speak
// everything" commands). It relays the on/off as a speech_mode message to this
// client AND persists it through the shared-settings catalogue (a synced record),
// broadcasting the updated settings so every client's audio behavior stays in sync.
// Arg "off" turns it off; anything else turns it on.

// doSummaryOnly toggles summary-only speech (the voice "summary only" / "speak
// everything" commands). It relays the on/off as a speech_mode message to this
// client AND persists it through the shared-settings catalogue (a synced record),
// broadcasting the updated settings so every client's audio behavior stays in sync.
// Arg "off" turns it off; anything else turns it on.
func (c *conn) doSummaryOnly(arg string) {
	on := arg != "off"
	c.send(msgSpeechMode(on))
	if c.srv.persistSetting("summary_only", strconv.FormatBool(on), nowMillis()) {
		c.srv.broadcastSettings()
	}
	if on {
		c.send(msgSay("summary only — I'll beep through the steps and speak just the final result. say 'speak everything' to hear it all."))
	} else {
		c.send(msgSay("okay, I'll speak everything again."))
	}
}

// doUsage runs `/usage` (a full but lightweight claude invocation) in the
// background and returns the plan's usage report — the session/weekly percent-
// used numbers the TUI `/usage` shows. If speak, it also sends a short spoken
// summary (the "usage" voice command); a tap trigger stays silent and just fills
// the app's usage sheet.

// doRestart fires the configured restart command to rebuild and relaunch the
// server, picking up any new server code. The command (SPAWNER_RESTART_CMD) runs
// detached on the host — SSHing over and running deploy/rebuild-container.sh to
// rebuild the image and recreate this container — so the process is replaced out
// from under us and the app auto-reconnects once the fresh one is listening. Any authenticated client may
// trigger this; the trust boundary is the same as spawning arbitrary commands.
// Reports back if restart isn't configured (SPAWNER_RESTART_CMD unset) instead of
// pretending it worked.
// mode selects what the restart command does: "build" rebuilds the image only and
// leaves the running container in place (no bounce, so the caller's live session keeps
// going); "bounce" recreates the container from the existing image without recompiling;
// "rebuild" (the default, and empty from the voice command) builds then recreates.
// Progress is reported to every client as `restart_status` (started → finished /
// failed): the host command is setsid-detached, so its own return says nothing about
// whether the build succeeded — the driver watches the host's status file for that.
// A finished `build` sets the server's pending-bounce bit (image staged, container
// still on the old one), which the app also reads from `hello_ok` on reconnect; a
// finished bounce/rebuild clears it.
func (c *conn) doRestart(mode string) {
	go func() {
		if err := c.srv.driver.Restart(context.Background(), mode, func(phase, errText string) {
			c.srv.noteRestartPhase(mode, phase, errText)
		}); err != nil {
			c.fail("restart_failed", err.Error())
			return
		}
		var text string
		switch mode {
		case "build":
			text = "rebuilding the server image — the running server keeps going; tap restart when it's ready."
		case "bounce":
			text = "restarting the server — back in a moment."
		default:
			text = "rebuilding and restarting the server — back in a moment."
		}
		c.srv.broadcast(msgSay(text))
	}()
}

// noteRestartPhase records a restart's progress and broadcasts it as
// `restart_status`. It owns the pending-bounce bit: a finished `build` staged a new
// image the running container isn't on yet (set it), while a finished bounce/rebuild
// means the container now runs the current image (clear it). A finished build or any
// failure also gets a spoken line, since those are the phases a listening user acts
// on; started is silent (doRestart already said what it's doing).
func (s *Server) noteRestartPhase(mode, phase, errText string) {
	if mode == "" {
		mode = "rebuild"
	}
	s.restartMu.Lock()
	switch {
	case phase != "finished":
		// leave the bit as it is; a failed build stages nothing new
	case mode == "build":
		s.restartPending = true
	default:
		s.restartPending = false
	}
	pending := s.restartPending
	s.restartMu.Unlock()

	s.broadcast(msgRestartStatus(mode, phase, errText, pending))
	switch {
	case phase == "failed":
		s.broadcast(msgSay("the server " + mode + " failed: " + errText))
	case phase == "finished" && mode == "build":
		s.broadcast(msgSay("the new server image is ready — tap restart when you want to switch to it."))
	}
}

// restartPendingBit reports whether an image has been built that the running
// container isn't on yet, for `hello_ok` on (re)connect.
func (s *Server) restartPendingBit() bool {
	s.restartMu.Lock()
	defer s.restartMu.Unlock()
	return s.restartPending
}

// abortTurn cancels the running turn on the attached session (kills the claude
// child). The turn's goroutine then delivers a `turn_stopped` to clear the app.

// abortTurn cancels the running turn on the attached session (kills the claude
// child). The turn's goroutine then delivers a `turn_stopped` to clear the app.
func (c *conn) abortTurn() {
	if c.attached != nil && c.srv.cancelTurn(c.attached.SessionID) {
		c.send(msgSay("stopping that."))
		return
	}
	c.send(msgSay("nothing running to stop."))
}

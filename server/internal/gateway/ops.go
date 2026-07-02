package gateway

import (
	"fmt"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/session"
)

// dispatch handles an immediate (push-to-talk / typed) utterance when no dialog
// is active: a control command, or dictation to the attached session.
func (c *conn) dispatch(text string) {
	rest, hadWake := command.StripWake(text)

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
	} else {
		c.send(msgSay("didn't catch that, bud. try 'spawn a new session'."))
	}
}

// runCommand executes a recognized control command, returning false if the
// intent is Unknown (so the caller decides what to do). Shared by the immediate
// path (dispatch) and the hands-free commit path (commitMessage).
func (c *conn) runCommand(intent command.Intent) bool {
	switch intent.Kind {
	case command.Spawn:
		c.startSpawn(intent.New, intent.Location)
	case command.List:
		c.doList()
	case command.Attach:
		c.doAttach(intent.Arg)
	case command.Detach:
		c.doDetach()
	case command.Kill:
		c.doKill(intent.Arg)
	case command.Status:
		c.doStatus()
	case command.Stop:
		c.send(msgStopSpeaking())
	case command.Cancel:
		c.send(msgSay("nothing to cancel, bud."))
	case command.Help:
		c.send(msgSay(commandHelp))
	case command.ReadLast:
		c.send(msgReadLast(intent.Count))
	default:
		return false
	}
	return true
}

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.
func (c *conn) serveHistory(name string, before *int, limit int) {
	s := c.srv.store.Get(name)
	if s == nil {
		c.send(msgError("no_session", "no such session: "+name))
		return
	}
	msgs, err := session.ReadTranscript(s.TranscriptPath())
	if err != nil {
		c.send(msgError("history_failed", err.Error()))
		return
	}
	b := -1
	if before != nil {
		b = *before
	}
	page, more := session.HistoryPage(msgs, b, limit)
	c.send(msgHistory(name, page, more))
}

// commandHelp is spoken + shown when the user asks "hey buddy help".
const commandHelp = "here's what I know, bud: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, cancel message, and help. " +
	"say hey buddy, then the command, then your end token."

// sendSessionList pushes the current sessions to the app without speaking (used
// for the sidebar / silent refreshes).
func (c *conn) sendSessionList() {
	sessions := c.srv.store.List()
	views := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, sessionView{Name: s.Name, Dir: s.Dir})
	}
	c.send(msgSessionList(views))
}

// doList is the spoken "list sessions" voice command.
func (c *conn) doList() {
	c.sendSessionList()
	switch sessions := c.srv.store.List(); len(sessions) {
	case 0:
		c.send(msgSay("no sessions yet, bud."))
	case 1:
		c.send(msgSay("one session: " + sessions[0].Name + "."))
	default:
		c.send(msgSay(fmt.Sprintf("%d sessions.", len(sessions))))
	}
}

func (c *conn) doAttach(name string) {
	if name == "" {
		c.send(msgSay("which session, bud?"))
		return
	}
	s := c.srv.store.Get(name)
	if s == nil {
		c.send(msgError("no_session", "no session named "+name))
		return
	}
	if c.attached != nil {
		c.srv.unbindJob(c.attached.Name)
	}
	c.clearBuffer() // fresh message buffer for the new session
	c.attached = s
	c.send(msgAttached(s.Name))
	c.send(msgSay("attached to " + s.Name + ". go ahead, bud."))
	// Catch up on a job that may still be running (or finished while we were gone).
	c.srv.bindJob(s.Name, c.jobSink())
}

func (c *conn) doDetach() {
	if c.attached == nil {
		c.send(msgSay("you're not attached to anything, bud."))
		return
	}
	c.srv.unbindJob(c.attached.Name)
	c.clearBuffer()
	c.attached = nil
	c.send(msgDetached())
	c.send(msgSay("detached."))
}

// removeSession deletes a session: closes any babysit pane, detaches if we're on
// it, and pushes the refreshed list. Returns false (with an error) if unknown.
func (c *conn) removeSession(name string) bool {
	s := c.srv.store.Get(name)
	if s == nil {
		c.send(msgError("no_session", "no session named "+name))
		return false
	}
	if open, _ := c.srv.babysit.Exists(c.ctx, s.Name); open {
		_ = c.srv.babysit.Close(c.ctx, s.Name)
	}
	if err := c.srv.store.Delete(s.Name); err != nil {
		c.send(msgError("internal", err.Error()))
		return false
	}
	if c.attached != nil && c.attached.Name == s.Name {
		c.attached = nil
		c.send(msgDetached())
	}
	c.srv.dropJob(s.Name)
	c.sendSessionList()
	return true
}

// doKill is the spoken "kill session" voice command.
func (c *conn) doKill(name string) {
	if name == "" {
		c.send(msgSay("which session should I kill, bud?"))
		return
	}
	if c.removeSession(name) {
		c.send(msgSay("killed " + name + "."))
	}
}

// doDelete is the app's delete action (no speech).
func (c *conn) doDelete(name string) {
	if name == "" {
		c.send(msgError("bad_message", "need a session name"))
		return
	}
	c.removeSession(name)
}

func (c *conn) doRename(old, newName string) {
	newName = sanitizeName(newName)
	if old == "" || newName == "" {
		c.send(msgError("bad_message", "need name and new_name"))
		return
	}
	if err := c.srv.store.Rename(old, newName); err != nil {
		c.send(msgError("rename_failed", err.Error()))
		return
	}
	// Follow the rename if we're attached to it.
	if c.attached != nil && c.attached.Name == old {
		c.attached = c.srv.store.Get(newName)
	}
	c.sendSessionList() // push the refreshed list back to the app (quietly)
}

func (c *conn) doStatus() {
	if c.attached == nil {
		c.send(msgSay("you're not attached to anything, bud."))
		return
	}
	c.send(msgSay("you're attached to " + c.attached.Name + " in " + c.attached.Dir + "."))
}

// dictate runs one Claude turn for the attached session as a background job that
// outlives this connection — so a long job keeps running if the app disconnects,
// and its result is delivered on reconnect. Only one turn per session at a time.
func (c *conn) dictate(text string) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first, bud."))
		return
	}
	if !c.srv.startTurn(c.attached, text, c.jobSink()) {
		c.send(msgSay("still working on the last one, bud."))
	}
}

// affirmative / negative recognize yes/no style dialog replies.
func affirmative(text string) bool {
	r, _ := command.StripWake(text)
	return command.Parse(r).Kind != command.Cancel &&
		containsAny(r, "yes", "yeah", "yep", "yup", "sure", "do it", "please", "go ahead", "ok", "okay")
}

func negative(text string) bool {
	r, _ := command.StripWake(text)
	return containsAny(r, "no", "nope", "nah", "don't", "do not", "scrap", "skip")
}

// newSession builds a durable record with a generated session_id, ensuring a
// unique name derived from base.
func (c *conn) newSession(base, dir string) (*session.Session, error) {
	id, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	name := c.srv.uniqueName(base)
	return &session.Session{Name: name, Dir: dir, SessionID: id}, nil
}

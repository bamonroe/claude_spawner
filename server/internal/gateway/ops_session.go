package gateway

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/bam/claude_spawner/server/internal/session"
)

// doList is the spoken "list sessions" voice command.
func (c *conn) doList() {
	c.sendSessionList()
	// Speak the names from the unified (all-machine) list, newest first, using
	// custom registry names where set. Cap the spoken count so a machine with
	// dozens of sessions doesn't read a novel.
	found, err := c.srv.driver.DiscoverSessions("")
	if err != nil {
		c.send(msgSay("couldn't list sessions."))
		return
	}
	byDir := map[string]string{}
	for _, s := range c.srv.store.List() {
		byDir[s.Dir] = s.Name
	}
	names := make([]string, 0, len(found))
	for _, d := range found {
		if n, ok := byDir[d.Dir]; ok {
			names = append(names, n)
		} else {
			names = append(names, sanitizeName(filepath.Base(d.Dir)))
		}
	}
	switch len(names) {
	case 0:
		c.send(msgSay("no sessions yet."))
	case 1:
		c.send(msgSay("one session: " + names[0] + "."))
	default:
		const maxSpoken = 8
		spoken, more := names, 0
		if len(spoken) > maxSpoken {
			more, spoken = len(spoken)-maxSpoken, spoken[:maxSpoken]
		}
		msg := fmt.Sprintf("%d sessions: %s", len(names), strings.Join(spoken, ", "))
		if more > 0 {
			msg += fmt.Sprintf(", and %d more", more)
		}
		c.send(msgSay(msg + "."))
	}
}

// doAttach attaches to a session. silent suppresses the spoken confirmation and
// the "still working" catch-up nudge — used for the app's auto-attach on
// reconnect, so a network blip doesn't re-announce "attached… go ahead."
// (a finished turn's buffered result is still delivered regardless).
// doAttachBy attaches by stable session_id when one is given (the app's preferred
// handle — it survives renames and is the same across servers), falling back to
// the name otherwise. Resolving id->current name here lets the app auto-reattach
// across a server change where the same session carries a different name.

// sendSessionList pushes the current sessions to the app without speaking (used
// for the sidebar / silent refreshes).
func (c *conn) sendSessionList() {
	sessions := c.srv.store.List()
	views := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, sessionView{Name: s.Name, Dir: s.Dir, Target: sandboxTarget(s), Agent: s.Agent, Model: s.Model, Profile: s.Profile})
	}
	c.send(msgSessionList(views))
}

// doList is the spoken "list sessions" voice command.

// sandboxTarget returns the session's target string only when it's a sandbox
// session (the non-default target the app badges); "" for host sessions.
func sandboxTarget(s *session.Session) string {
	if s.Target == session.TargetSandbox {
		return string(s.Target)
	}
	return ""
}

// sendSessionList pushes the current sessions to the app without speaking (used
// for the sidebar / silent refreshes).

// doAttach attaches to a session. silent suppresses the spoken confirmation and
// the "still working" catch-up nudge — used for the app's auto-attach on
// reconnect, so a network blip doesn't re-announce "attached… go ahead."
// (a finished turn's buffered result is still delivered regardless).
// doAttachBy attaches by stable session_id when one is given (the app's preferred
// handle — it survives renames and is the same across servers), falling back to
// the name otherwise. Resolving id->current name here lets the app auto-reattach
// across a server change where the same session carries a different name.
func (c *conn) doAttachBy(sessionID, name string, silent bool) {
	if sessionID != "" {
		if s := c.srv.store.GetBySessionID(sessionID); s != nil {
			name = s.Name
		}
	}
	c.doAttach(name, silent)
}

// selectClientSession makes the connection follow the app's declared active
// session for one utterance. Old clients omit sessionID and keep the historical
// server-side attachment behavior. New clients send the session_id they are
// visibly focused on, so dictation follows the user's app view even if this
// connection's attachment state is stale.

// selectClientSession makes the connection follow the app's declared active
// session for one utterance. Old clients omit sessionID and keep the historical
// server-side attachment behavior. New clients send the session_id they are
// visibly focused on, so dictation follows the user's app view even if this
// connection's attachment state is stale.
func (c *conn) selectClientSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return true
	}
	if c.attached != nil && c.attached.SessionID == sessionID {
		return true
	}
	// A "clear"/"compress" rotates the attached session's id and retires (ForgetID)
	// the old one, but the app keeps routing by that pre-rotation id until it sees a
	// fresh `attached` — which a context_reset doesn't send. So a stale id that is a
	// PriorID of the session we're already on just means "the session I'm attached
	// to"; stay on it instead of erroring with "that session is gone."
	if c.attached != nil && c.attached.HasPriorID(sessionID) {
		return true
	}
	s := c.srv.store.GetBySessionID(sessionID)
	if s == nil {
		c.send(msgSay("that session is gone."))
		return false
	}
	if c.attached != nil {
		c.prevSessionID = c.attached.SessionID
		c.srv.unbindJob(c, c.attached.SessionID)
	}
	c.setAttached(s)
	c.send(msgAttached(s, c.srv.driver.LastContextUsage(s.Agent, s.Host, s.TranscriptIDs())))
	c.srv.bindJob(c, s, true)
	return true
}

func (c *conn) doAttach(name string, silent bool) {
	if name == "" {
		c.send(msgSay("which session?"))
		return
	}
	s := c.resolveSession(name)
	if s == nil {
		if !silent {
			c.send(msgSay("no session named " + name + "."))
		}
		return
	}
	if c.attached != nil {
		// Remember the session we're leaving so "swap" can toggle back to it —
		// but only on a genuine move to a different session (re-attaching to the
		// same one mustn't make swap a no-op).
		if c.attached.SessionID != s.SessionID {
			c.prevSessionID = c.attached.SessionID
		}
		c.srv.unbindJob(c, c.attached.SessionID)
	}
	c.clearBuffer() // fresh message buffer for the new session
	c.setAttached(s)
	c.send(msgAttached(s, c.srv.driver.LastContextUsage(s.Agent, s.Host, s.TranscriptIDs())))
	if !silent {
		c.send(msgSay("attached to " + s.Name + "."))
	}
	// Catch up on a job that may still be running (or finished while we were gone).
	c.srv.bindJob(c, s, silent)
}

// matchKey normalizes a spoken/stored name or dir for fuzzy voice matching:
// lowercase, drop a leading "claude-", keep only letters+digits (so "attach to
// bam store" → "bamstore" matches a session named "bam-store" or "claude-bam-store").

// matchKey normalizes a spoken/stored name or dir for fuzzy voice matching:
// lowercase, drop a leading "claude-", keep only letters+digits (so "attach to
// bam store" → "bamstore" matches a session named "bam-store" or "claude-bam-store").
func matchKey(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "claude-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// resolveSession finds the session a spoken name refers to: an exact/fuzzy match
// in the registry (by name or dir basename), else a fuzzy match against sessions
// on disk — which it adopts into the registry so it can be driven. nil if none.

// resolveSession finds the session a spoken name refers to: an exact/fuzzy match
// in the registry (by name or dir basename), else a fuzzy match against sessions
// on disk — which it adopts into the registry so it can be driven. nil if none.
func (c *conn) resolveSession(spoken string) *session.Session {
	key := matchKey(spoken)
	if key == "" {
		return nil
	}
	if s := c.srv.store.Get(spoken); s != nil { // exact first
		return s
	}
	for _, s := range c.srv.store.List() {
		if matchKey(s.Name) == key || matchKey(filepath.Base(s.Dir)) == key {
			return s
		}
	}
	found, _ := c.srv.driver.DiscoverSessions("")
	for _, d := range found {
		if matchKey(filepath.Base(d.Dir)) == key {
			if rec, err := c.srv.registerDiscovered(d.SessionID, d.Dir); err == nil {
				c.sendSessionList()
				return rec
			}
		}
	}
	return nil
}

// doSetWhisperModel changes a resident whisper server's model (server-global) —
// the fast (draft/detection) server when fast is set, else the accurate one.
// The /load blocks (a big model takes seconds), so run it off the read loop; on
// success, broadcast the new models to every client, else report the error.

func (c *conn) doDetach() {
	if c.attached == nil {
		c.send(msgSay("you're not attached to anything."))
		return
	}
	// Detaching still leaves a "previous" session so a following "swap" jumps
	// straight back to what you were just in.
	c.prevSessionID = c.attached.SessionID
	c.srv.unbindJob(c, c.attached.SessionID)
	c.clearBuffer()
	c.setAttached(nil)
	c.send(msgDetached())
	c.send(msgSay("detached."))
}

// doSwap toggles back to the session attached just before the current one — a
// two-way jump between the two most-recent sessions. Shared by the voice "swap"
// command and the app's right-to-left swipe. doAttach records the outgoing
// session as the new previous, so repeated swaps ping-pong between the pair.

// doSwap toggles back to the session attached just before the current one — a
// two-way jump between the two most-recent sessions. Shared by the voice "swap"
// command and the app's right-to-left swipe. doAttach records the outgoing
// session as the new previous, so repeated swaps ping-pong between the pair.
func (c *conn) doSwap() {
	if c.prevSessionID == "" {
		c.send(msgSay("no previous session to swap to."))
		return
	}
	// GetByAnyID, not GetBySessionID: if the previous session was cleared or
	// compressed since we left it, prevSessionID is now one of its PriorIDs (the
	// live id rotated), so a plain byID lookup would miss a session that's still
	// very much alive and wrongly report "the previous session is gone."
	prev := c.srv.store.GetByAnyID(c.prevSessionID)
	if prev == nil { // the previous session was killed since we left it
		c.prevSessionID = ""
		c.send(msgSay("the previous session is gone."))
		return
	}
	if c.attached != nil && c.attached.SessionID == prev.SessionID {
		return // already there; nothing to toggle
	}
	c.doAttachBy(prev.SessionID, prev.Name, false)
}

// doClear rotates the attached session's Claude context: the current session_id is
// retired onto PriorIDs (its transcript kept on disk for the app's history view)
// and a fresh session_id takes over, so the next dictation starts Claude with an
// empty context instead of re-reading — and re-billing — the whole conversation.
// The full history stays visible via serveHistory's chain read; Claude just stops
// seeing it. Shared by the voice command and the app action.

// doClear rotates the attached session's Claude context: the current session_id is
// retired onto PriorIDs (its transcript kept on disk for the app's history view)
// and a fresh session_id takes over, so the next dictation starts Claude with an
// empty context instead of re-reading — and re-billing — the whole conversation.
// The full history stays visible via serveHistory's chain read; Claude just stops
// seeing it. Shared by the voice command and the app action.
func (c *conn) doClear() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	s := c.attached
	if !s.Started {
		c.send(msgSay("nothing to clear yet."))
		return
	}
	if c.srv.isBusy(s.SessionID) {
		c.send(msgSay("still working on the last one — try clearing when it's done."))
		return
	}
	newID, err := session.NewSessionID()
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	oldID := s.SessionID
	s.PriorIDs = append(s.PriorIDs, s.SessionID)
	s.SessionID = newID
	s.Started = false
	s.AskPrimed = false  // fresh context: re-prime the ask instruction on the next turn
	s.JobsPrimed = false // ditto for the background-job instruction (Jobs/PendingNotes survive: a bg job outlives a clear)
	s.PendingSeed = ""   // a clear means truly empty context — drop any compress seed
	if err := c.srv.store.Put(s); err != nil {
		c.fail("internal", err.Error())
		return
	}
	// The session_id rotated: move the hub (holds attached sinks) and the id index
	// onto the new id so later turns still reach the attached devices.
	c.srv.rekeyJob(oldID, newID)
	if ferr := c.srv.store.ForgetID(oldID); ferr != nil {
		log.Printf("forget rotated id %s: %v", oldID, ferr)
	}
	c.clearBuffer()
	// One self-describing reset: it carries the rotated session_id, so the app
	// re-keys and refreshes this session's rows off it — no `attached` re-emit.
	c.send(msgContextReset(s.Name, s.SessionID))
	c.send(msgSay("cleared. starting fresh — your history is still here."))
}

// doListModels speaks the models the attached session's AI backend offers, in
// order, so the user can pick one by NUMBER ("use model 2"). Ordinal selection
// keeps hard-to-say model names (e.g. Codex's gpt-5.5 reasoning presets) out of
// the voice path. Marks the session's current model.

// removeSession deletes a session: detaches if we're on it, drops its job, and
// pushes the refreshed list. Returns false (with an error) if unknown.
func (c *conn) removeSession(name string) bool {
	s := c.srv.store.Get(name)
	if s == nil {
		c.fail("no_session", "no session named "+name)
		return false
	}
	// A delete now wipes the session's transcript too, so refuse while an
	// interactive claude is live in that directory — deleting a file it's writing
	// would corrupt it (same guard as the app's delete_discovered path).
	if c.srv.tmuxMgr.ClaudeDirs(c.ctx)[s.Dir] {
		c.fail("session_active", "that session is live in a terminal — close it there first")
		return false
	}
	if c.attached != nil && c.attached.Name == s.Name {
		c.setAttached(nil)
		c.send(msgDetached())
	}
	// Purge every on-disk trace of the session (transcript, sidecar, per-session
	// state) across every backend it ran — not just the registry record — so nothing
	// about it is left on disk. Best-effort: a purge error still drops the record below.
	if _, err := c.srv.driver.DeleteSessionAll(s); err != nil {
		log.Printf("delete session %s transcripts: %v", s.Name, err)
	}
	if err := c.srv.store.Delete(s.Name); err != nil {
		c.fail("internal", err.Error())
		return false
	}
	c.removeSandbox(s) // destroy the session's container, if any
	c.srv.dropJob(s.SessionID)
	c.sendSessionList()
	return true
}

// doKill is the spoken "kill session" voice command.

// doKill is the spoken "kill session" voice command.
func (c *conn) doKill(name string) {
	if name == "" {
		c.send(msgSay("which session should I kill?"))
		return
	}
	if c.removeSession(name) {
		c.send(msgSay("killed " + name + "."))
	}
}

// doDelete is the app's delete action (no speech).

// doDelete is the app's delete action (no speech).
func (c *conn) doDelete(name string) {
	if name == "" {
		c.fail("bad_message", "need a session name")
		return
	}
	c.removeSession(name)
}

// doRename renames a session by explicit old→new name (the `rename` wire
// message). Returns whether the rename succeeded so voice callers can decide
// whether to speak a confirmation.

// doRename renames a session by explicit old→new name (the `rename` wire
// message). Returns whether the rename succeeded so voice callers can decide
// whether to speak a confirmation.
func (c *conn) doRename(old, newName string) bool {
	newName = sanitizeName(newName)
	if old == "" || newName == "" {
		c.fail("bad_message", "need name and new_name")
		return false
	}
	if err := c.srv.store.Rename(old, newName); err != nil {
		c.fail("rename_failed", err.Error())
		return false
	}
	// Rename mutates the record in place, so every connection attached to this
	// session — the initiator plus any other device the user has on it (they run a
	// phone AND a tablet at once) — holds this same *Session pointer. The job hub and
	// in-flight state are keyed by session_id (stable across a rename), so nothing
	// there needs re-keying.
	rec := c.srv.store.Get(newName)
	// Push the `renamed` title update to EVERY connection attached to this session,
	// not just the initiator, so each client updates its attached-session title in
	// place (matching by the stable session_id) instead of inferring the rename from
	// a later discovered-list diff.
	if rec != nil {
		c.srv.broadcastRenamed(rec, old, newName)
	}
	c.sendSessionList() // push the refreshed list back to the app (quietly)
	return true
}

// doRenameCurrent renames the currently-attached session (the `rename` voice
// command). Unlike the wire `rename` message it has no explicit "old" name — it
// always targets whatever session you're attached to — and it speaks a friendly
// confirmation, since it's driven by voice.

// doRenameCurrent renames the currently-attached session (the `rename` voice
// command). Unlike the wire `rename` message it has no explicit "old" name — it
// always targets whatever session you're attached to — and it speaks a friendly
// confirmation, since it's driven by voice.
func (c *conn) doRenameCurrent(newName string) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first, then tell me what to call it."))
		return
	}
	name := sanitizeName(newName)
	if strings.TrimSpace(newName) == "" {
		c.send(msgSay("what should I call it?"))
		return
	}
	old := c.attached.Name
	if name == old {
		c.send(msgSay("it's already called " + old + "."))
		return
	}
	if c.doRename(old, name) {
		c.send(msgSay("renamed " + old + " to " + name + "."))
	}
}

func (c *conn) doStatus() {
	if c.attached == nil {
		c.send(msgSay("you're not attached to anything."))
		return
	}
	c.send(msgSay("you're attached to " + c.attached.Name + " in " + c.attached.Dir + "."))
}

// dictate runs one Claude turn for the attached session as a background job that
// outlives this connection — so a long job keeps running if the app disconnects,
// and its result is delivered on reconnect. Only one turn per session at a time.

// newSession builds a durable record with a generated session_id, ensuring a
// unique name derived from base.
func (c *conn) newSession(base, dir string, target session.Target, agentID, profileID string) (*session.Session, error) {
	id, err := session.NewSessionID()
	if err != nil {
		return nil, err
	}
	s := &session.Session{Name: c.srv.uniqueName(base), Dir: dir, SessionID: id, Target: target}
	// Stamp the AI backend and its default model — spawn chooses the model for you
	// ("use model N" switches it later). agentID empty/unknown resolves to the
	// default backend (Claude), so the visual picker (no backend choice yet) and
	// old callers get Claude.
	ag := c.srv.driver.Registry().Resolve(agentID)
	s.Agent, s.Model = ag.ID, c.srv.driver.ProviderSettings().DefaultModel(ag)
	reg := c.srv.driver.ProfileRegistry()
	if p := reg.Resolve(profileID); p != nil && p.Name != reg.DefaultName() {
		s.Profile = p.Name
	}
	if target == session.TargetSandbox {
		cn, err := session.NewContainerName()
		if err != nil {
			return nil, err
		}
		s.Container = cn
	} else {
		// A host-target session always carries an explicit host; default an
		// unspecified spawn (voice dialog, legacy clients) to the loopback host
		// rather than leaving it empty, which the SSH executor rejects. A caller that
		// named a host (the spawn picker) overrides this afterwards.
		s.Host = session.LocalHost
	}
	return s, nil
}

// ensureSandbox best-effort starts a sandbox session's persistent container at
// spawn. A failure (e.g. the runtime being unavailable) is logged but does NOT
// block the spawn — the first turn re-runs Ensure and surfaces a hard error then.

// ensureSandbox best-effort starts a sandbox session's persistent container at
// spawn. A failure (e.g. the runtime being unavailable) is logged but does NOT
// block the spawn — the first turn re-runs Ensure and surfaces a hard error then.
func (c *conn) ensureSandbox(s *session.Session) {
	if err := c.srv.driver.EnsureContainer(c.ctx, s); err != nil {
		log.Printf("sandbox ensure for %s: %v", s.Name, err)
	}
}

// removeSandbox best-effort destroys a session's persistent container on delete.
// Logged, never fatal — a runtime hiccup must not block removing the record.

// removeSandbox best-effort destroys a session's persistent container on delete.
// Logged, never fatal — a runtime hiccup must not block removing the record.
func (c *conn) removeSandbox(s *session.Session) {
	if err := c.srv.driver.RemoveContainer(c.ctx, s); err != nil {
		log.Printf("sandbox remove for %s: %v", s.Name, err)
	}
}

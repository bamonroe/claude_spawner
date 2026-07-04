package gateway

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

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
		c.send(msgSay("didn't catch that. try 'spawn a new session'."))
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
		c.doAttach(intent.Arg, false) // voice attach announces
	case command.Detach:
		c.doDetach()
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
	default:
		return false
	}
	return true
}

// doUsage runs `/usage` (a full but lightweight claude invocation) in the
// background and returns the plan's usage report — the session/weekly percent-
// used numbers the TUI `/usage` shows. If speak, it also sends a short spoken
// summary (the "usage" voice command); a tap trigger stays silent and just fills
// the app's usage sheet.
func (c *conn) doUsage(speak bool) {
	if speak {
		c.send(msgSay("checking your usage — one sec."))
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 90*time.Second)
		defer cancel()
		text, err := c.srv.driver.Usage(ctx)
		if err != nil {
			c.send(msgError("usage_failed", err.Error()))
			return
		}
		sp, sr, wp, wr := parseUsage(text)
		c.send(msgUsage(sp, sr, wp, wr, strings.TrimSpace(text)))
		if speak {
			c.send(msgSay(usageSummary(sp, wp)))
		}
	}()
}

var (
	reUsageSession = regexp.MustCompile(`(?i)current session:\s*(\d+)% used(?:\s*·\s*resets\s*([^\n]+))?`)
	reUsageWeek    = regexp.MustCompile(`(?i)current week \(all models\):\s*(\d+)% used(?:\s*·\s*resets\s*([^\n]+))?`)
)

// parseUsage pulls the session and weekly percent-used headline out of a /usage
// report. Returns -1 for a percent it couldn't find and "" for a missing reset;
// the app shows the full text verbatim regardless, so this is a best-effort
// headline, not the whole story.
func parseUsage(text string) (sessionPct int, sessionReset string, weekPct int, weekReset string) {
	sessionPct, weekPct = -1, -1
	if m := reUsageSession.FindStringSubmatch(text); m != nil {
		sessionPct = atoiSafe(m[1])
		sessionReset = cleanReset(m[2])
	}
	if m := reUsageWeek.FindStringSubmatch(text); m != nil {
		weekPct = atoiSafe(m[1])
		weekReset = cleanReset(m[2])
	}
	return
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return n
}

// cleanReset trims the reset string and drops a trailing " (timezone)" note so
// "Jul 4, 9:59am (America/New_York)" reads as "Jul 4, 9:59am".
func cleanReset(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, " ("); i > 0 {
		s = s[:i]
	}
	return s
}

// usageSummary is the short spoken line for the "usage" voice command.
func usageSummary(sessionPct, weekPct int) string {
	switch {
	case sessionPct < 0 && weekPct < 0:
		return "couldn't read your usage."
	case sessionPct < 0:
		return fmt.Sprintf("you've used %d%% of this week's limit.", weekPct)
	case weekPct < 0:
		return fmt.Sprintf("you've used %d%% of this session's limit.", sessionPct)
	default:
		return fmt.Sprintf("you've used %d%% of this session and %d%% of the week.", sessionPct, weekPct)
	}
}

// doDiscover scans ~/.claude/projects for all Claude sessions (spawner-created
// or not) and returns them, flagged with whether they're already registered and
// whether an interactive claude is live in tmux at that directory.
func (c *conn) doDiscover() {
	found, err := session.DiscoverSessions()
	if err != nil {
		c.send(msgError("discover_failed", err.Error()))
		return
	}
	active := c.srv.tmuxMgr.ClaudeDirs(c.ctx)
	registered := c.srv.store.List()
	// index registry by DIRECTORY so custom (renamed) names carry over even if the
	// dir's newest session_id differs from the pinned one.
	byDir := map[string]*session.Session{}
	for _, s := range registered {
		byDir[s.Dir] = s
	}
	views := make([]discoveredView, 0, len(found)+1)
	seenDir := map[string]bool{}
	for _, d := range found {
		seenDir[d.Dir] = true
		name, registered := sanitizeName(filepath.Base(d.Dir)), false
		if s := byDir[d.Dir]; s != nil {
			name, registered = s.Name, true
		}
		views = append(views, discoveredView{
			Name: name, Dir: d.Dir, SessionID: d.SessionID,
			LastActive: d.LastActive, Active: active[d.Dir], Registered: registered,
			Busy: registered && c.srv.isBusy(name),
		})
	}
	// Include registered sessions that have no transcript yet (just-spawned) so
	// the merged list is complete.
	for _, s := range registered {
		if !seenDir[s.Dir] {
			views = append(views, discoveredView{
				Name: s.Name, Dir: s.Dir, SessionID: s.SessionID,
				LastActive: 0, Active: active[s.Dir], Registered: true,
				Busy: c.srv.isBusy(s.Name),
			})
		}
	}
	c.send(msgDiscovered(views))
}

// doRenameDiscovered gives a discovered session a custom name: registers it (by
// dir, without attaching) if needed, then renames the record. Refreshes lists.
func (c *conn) doRenameDiscovered(sessionID, dir, newName string) {
	if dir == "" || sanitizeName(newName) == "" {
		c.send(msgError("bad_rename", "need dir and a valid new_name"))
		return
	}
	rec := c.srv.store.GetByDir(dir)
	if rec == nil {
		var err error
		if rec, err = c.srv.registerDiscovered(sessionID, dir); err != nil {
			c.send(msgError("internal", err.Error()))
			return
		}
	}
	c.doRename(rec.Name, newName)
	c.doDiscover()
}

// doAdopt registers a discovered Claude session (by session_id + dir) into the
// spawner store and attaches to it, so the app can drive/view it via --resume.
// If the session_id is already registered, it just attaches.
func (c *conn) doAdopt(sessionID, dir string) {
	if sessionID == "" || dir == "" {
		c.send(msgError("bad_adopt", "adopt needs session_id and dir"))
		return
	}
	if s := c.srv.store.GetBySessionID(sessionID); s != nil {
		c.doAttach(s.Name, false)
		return
	}
	rec, err := c.srv.registerDiscovered(sessionID, dir)
	if err != nil {
		c.send(msgError("internal", err.Error()))
		return
	}
	c.sendSessionList()
	c.doAttach(rec.Name, false)
}

// doDeleteDiscovered PERMANENTLY deletes a discovered session's Claude
// transcript from disk (and its registry record, if any). Refuses if the
// session is live in a terminal — deleting its transcript out from under a
// running claude would corrupt it. Refreshes the discover + session lists.
func (c *conn) doDeleteDiscovered(sessionID string) {
	if sessionID == "" {
		c.send(msgError("bad_delete", "need session_id"))
		return
	}
	path := session.TranscriptPathByID(sessionID)
	if path == "" {
		c.send(msgError("not_found", "no transcript for that session"))
		return
	}
	dir := session.TranscriptCwd(path)
	if c.srv.tmuxMgr.ClaudeDirs(c.ctx)[dir] {
		c.send(msgError("session_active", "that session is live in a terminal — close it there first"))
		return
	}
	if _, err := session.DeleteSessionsForDir(sessionID, dir); err != nil {
		c.send(msgError("internal", err.Error()))
		return
	}
	// Drop any registry records for this directory too (detach if attached).
	for _, s := range c.srv.store.List() {
		if s.Dir == dir {
			if c.attached != nil && c.attached.Name == s.Name {
				c.doDetach()
			}
			_ = c.srv.store.Delete(s.Name)
			c.srv.dropJob(s.Name)
		}
	}
	c.sendSessionList()
	c.doDiscover() // refreshed list (the whole directory is gone now)
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
	msgs, err := session.ReadTranscriptChain(s.TranscriptIDs())
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
const commandHelp = "here's what I know: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, clear the context, compress the context, " +
	"stop the turn, cancel message, and help. say hey buddy, then the command, then your end token."

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
	// Speak the names from the unified (all-machine) list, newest first, using
	// custom registry names where set. Cap the spoken count so a machine with
	// dozens of sessions doesn't read a novel.
	found, err := session.DiscoverSessions()
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
		c.srv.unbindJob(c, c.attached.Name)
	}
	c.clearBuffer() // fresh message buffer for the new session
	c.attached = s
	c.send(msgAttached(s.Name))
	if !silent {
		c.send(msgSay("attached to " + s.Name + "."))
	}
	// Catch up on a job that may still be running (or finished while we were gone).
	c.srv.bindJob(c, s.Name, silent)
}

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
	found, _ := session.DiscoverSessions()
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

// doSetWhisperModel changes the resident whisper server's model (server-global).
// The /load blocks (a big model takes seconds), so run it off the read loop; on
// success, broadcast the new model to every client, else report the error.
func (c *conn) doSetWhisperModel(name string) {
	go func() {
		if err := c.srv.setWhisperModel(strings.TrimSpace(name)); err != nil {
			c.send(msgError("whisper_failed", err.Error()))
			return
		}
		c.srv.broadcastWhisperModel(c.srv.currentWhisperModel())
	}()
}

// doRestart tells every connected app the server is going down, then signals
// main() to exit so the process supervisor (the systemd unit) rebuilds and
// relaunches it — picking up any new server code. The app auto-reconnects once
// the fresh process is listening again. Any authenticated client may trigger
// this; the trust boundary is the same as spawning arbitrary commands.
func (c *conn) doRestart() {
	c.srv.broadcast(msgSay("restarting the server — back in a moment."))
	c.srv.RequestRestart()
}

// abortTurn cancels the running turn on the attached session (kills the claude
// child). The turn's goroutine then delivers a `turn_stopped` to clear the app.
func (c *conn) abortTurn() {
	if c.attached != nil && c.srv.cancelTurn(c.attached.Name) {
		c.send(msgSay("stopping that."))
		return
	}
	c.send(msgSay("nothing running to stop."))
}

func (c *conn) doDetach() {
	if c.attached == nil {
		c.send(msgSay("you're not attached to anything."))
		return
	}
	c.srv.unbindJob(c, c.attached.Name)
	c.clearBuffer()
	c.attached = nil
	c.send(msgDetached())
	c.send(msgSay("detached."))
}

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
	if c.srv.isBusy(s.Name) {
		c.send(msgSay("still working on the last one — try clearing when it's done."))
		return
	}
	newID, err := session.NewSessionID()
	if err != nil {
		c.send(msgError("internal", err.Error()))
		return
	}
	s.PriorIDs = append(s.PriorIDs, s.SessionID)
	s.SessionID = newID
	s.Started = false
	s.AskPrimed = false // fresh context: re-prime the ask instruction on the next turn
	s.PendingSeed = ""  // a clear means truly empty context — drop any compress seed
	if err := c.srv.store.Put(s); err != nil {
		c.send(msgError("internal", err.Error()))
		return
	}
	c.clearBuffer()
	c.send(msgSay("cleared. starting fresh — your history is still here."))
}

// doCompress compacts the attached session's Claude context: it asks Claude to
// summarize the conversation so far, then rotates to a fresh session_id whose
// next dictation is seeded with that summary. Unlike doClear (which drops context
// entirely), compress preserves it in condensed form — the Claude Code `/compact`
// analogue. The old transcript is kept on disk and history still spans the whole
// chain. The summary runs as a background turn (see startCompress); refused if a
// turn is in flight or no turn has run yet. Shared by the voice command and the
// app action.
func (c *conn) doCompress() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	s := c.attached
	if !s.Started {
		c.send(msgSay("nothing to compress yet."))
		return
	}
	if c.srv.isBusy(s.Name) {
		c.send(msgSay("still working on the last one — try compressing when it's done."))
		return
	}
	c.clearBuffer()
	if !c.srv.startCompress(s) {
		c.send(msgSay("still working on the last one."))
	}
}

// removeSession deletes a session: detaches if we're on it, drops its job, and
// pushes the refreshed list. Returns false (with an error) if unknown.
func (c *conn) removeSession(name string) bool {
	s := c.srv.store.Get(name)
	if s == nil {
		c.send(msgError("no_session", "no session named "+name))
		return false
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
		c.send(msgSay("which session should I kill?"))
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
	// Follow the rename if we're attached to it. The job hub is keyed by name, so
	// re-key it too (preserving its sinks + any in-flight turn/buffered result).
	if c.attached != nil && c.attached.Name == old {
		c.srv.renameJob(old, newName)
		c.attached = c.srv.store.Get(newName)
	}
	c.sendSessionList() // push the refreshed list back to the app (quietly)
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
func (c *conn) dictate(text string) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	prompt := text
	// A prior "compress" left a compacted summary of the old context to carry into
	// this fresh session_id; prepend it to the FIRST dictation so Claude continues
	// with that condensed context. startTurn clears PendingSeed once the turn lands.
	if c.attached.PendingSeed != "" && !c.attached.Started {
		prompt = seedPreamble(c.attached.PendingSeed) + prompt
	}
	if c.brief {
		// Opt-in: nudge Claude toward short, TTS-friendly replies. Only the prompt
		// to Claude carries the hint; the displayed/echoed transcript stays as spoken.
		prompt += "\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)"
	}
	// Interactive mode: append the ask instruction only until it's been primed for
	// this context. Claude retains it across turns via --resume, so re-sending it
	// every turn just burns tokens; a `clear` resets AskPrimed to re-prime.
	primeAsk := c.interactive && !c.attached.AskPrimed
	if primeAsk {
		prompt += askInstruction // let Claude ask instead of guessing (parsed back on reply)
	}
	if !c.srv.startTurn(c.attached, prompt, primeAsk) {
		c.send(msgSay("still working on the last one."))
		return
	}
	// Mirror the prompt onto any other devices attached to this session.
	c.srv.echoUserPrompt(c.attached.Name, text, c)
}

// seedPreamble frames a compress summary as leading context ahead of the user's
// first dictation on the rotated session, so Claude treats it as the recap of the
// prior conversation rather than as a new instruction.
func seedPreamble(seed string) string {
	return "[Continuing from a compacted session — recap of the conversation so far:]\n\n" +
		seed + "\n\n[End of recap. The user's message follows.]\n\n"
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

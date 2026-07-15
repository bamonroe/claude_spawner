package gateway

import (
	"context"
	"fmt"
	"log"
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
		c.doUsage(true, usageCalibrate) // voice command: show the report AND speak a summary
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
		c.doRestart(nil) // voice "restart" = full rebuild (nil default)
	default:
		return false
	}
	return true
}

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

// doSummaryOnly toggles summary-only speech. The state itself lives on the
// client (a persisted audio setting mirrored by the audio-settings switch), so
// the server holds none — it just relays the on/off as a speech_mode message
// and speaks a confirmation. Arg "off" turns it off; anything else turns it on.
func (c *conn) doSummaryOnly(arg string) {
	on := arg != "off"
	c.send(msgSpeechMode(on))
	if on {
		c.send(msgSay("summary only — I'll beep through the steps and speak just the final result. say 'speak everything' to hear it all."))
	} else {
		c.send(msgSay("okay, I'll speak everything again."))
	}
}

// usageAction selects what a /usage read does to the drift estimate once the
// real percentages are in hand.
type usageAction int

const (
	usageCalibrate usageAction = iota // passive EMA calibration (the default tap/voice check)
	usageSetBench                     // arm the manual two-point benchmark ("set" button)
	usageCalcBench                    // derive the rate directly from the benchmark ("calc" button)
)

// doUsage runs `/usage` (a full but lightweight claude invocation) in the
// background and returns the plan's usage report — the session/weekly percent-
// used numbers the TUI `/usage` shows. If speak, it also sends a short spoken
// summary (the "usage" voice command); a tap trigger stays silent and just fills
// the app's usage sheet. The action decides how the fresh real numbers feed the
// drift estimate: a normal check EMA-calibrates, while the "set"/"calc" buttons
// drive the manual two-point rate measurement (see usage.Estimator).
func (c *conn) doUsage(speak bool, action usageAction) {
	if speak {
		c.send(msgSay("checking your usage — one sec."))
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 90*time.Second)
		defer cancel()
		text, err := c.srv.driver.Usage(ctx)
		if err != nil {
			c.fail("usage_failed", err.Error())
			return
		}
		sp, sr, wp, wr := parseUsage(text)
		c.send(msgUsage(sp, sr, wp, wr, strings.TrimSpace(text)))
		now := time.Now().Unix()
		switch action {
		case usageSetBench:
			// Stamp this odometer/percent point; "calc" later measures from here.
			c.srv.broadcastUsageEstimate(c.srv.usage.SetBenchmark(float64(sp), float64(wp)))
			c.send(msgSay("benchmark set. burn some tokens, then hit calc."))
		case usageCalcBench:
			// Derive the tokens-per-percent rate directly from the benchmark interval.
			est, sessOK, weekOK := c.srv.usage.CalcBenchmark(now, float64(sp), float64(wp))
			c.srv.broadcastUsageEstimate(est)
			if sessOK || weekOK {
				c.send(msgSay("recalibrated the usage estimate from the benchmark."))
			} else {
				c.send(msgSay("not enough change since the benchmark yet — burn more tokens, then calc."))
			}
		default:
			// Snap the drift-live estimate back to these real numbers (and let it learn
			// the tokens-per-percent rate for the interval), then push to every client.
			c.srv.broadcastUsageEstimate(c.srv.usage.Calibrate(now, float64(sp), float64(wp)))
		}
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
	found, err := c.srv.driver.DiscoverSessions("")
	if err != nil {
		c.fail("discover_failed", err.Error())
		return
	}
	active := c.srv.tmuxMgr.ClaudeDirs(c.ctx)
	registered := c.srv.store.List()
	// last-active per dir, from discovery (used only to timestamp registered rows).
	discByDir := map[string]session.Discovered{}
	for _, d := range found {
		discByDir[d.Dir] = d
	}
	views := make([]discoveredView, 0, len(registered)+len(found))
	regDirs := map[string]bool{}
	// One row per REGISTERED session, keyed by its own session_id — no directory
	// collapse, so multiple sessions in the same dir are each individually visible
	// and separately renamable/deletable (this is what stops sessions hiding).
	for _, s := range registered {
		regDirs[s.Dir] = true
		views = append(views, discoveredView{
			Name: s.Name, Dir: s.Dir, SessionID: s.SessionID,
			LastActive: discByDir[s.Dir].LastActive, Active: active[s.Dir], Registered: true,
			Busy: c.srv.isBusy(s.SessionID), Target: sandboxTarget(s), Host: s.Host,
			Agent: s.Agent, Model: s.Model, Profile: s.Profile,
		})
	}
	// Unregistered sessions found on disk — one adoptable row per directory (these
	// aren't managed yet, so a per-dir entry to offer adoption is enough).
	for _, d := range found {
		if regDirs[d.Dir] {
			continue
		}
		views = append(views, discoveredView{
			Name: sanitizeName(filepath.Base(d.Dir)), Dir: d.Dir, SessionID: d.SessionID,
			LastActive: d.LastActive, Active: active[d.Dir], Registered: false,
			// Discovery scans this machine's disk, so an unregistered find is local.
			Host: session.LocalHost,
		})
	}
	c.send(msgDiscovered(views))
}

// doRenameDiscovered gives a discovered session a custom name. It resolves the
// target by SESSION_ID — the stable identity the app sends — not by directory, so
// with several sessions in one dir the rename lands on exactly the one picked
// (resolving by dir would hit whichever record won the byDir map). Registers the
// session (without attaching) if it isn't in the store yet, then renames.
func (c *conn) doRenameDiscovered(sessionID, dir, newName string) {
	if sanitizeName(newName) == "" {
		c.fail("bad_rename", "need a valid new_name")
		return
	}
	rec := c.srv.store.GetBySessionID(sessionID)
	if rec == nil {
		if dir == "" {
			c.fail("bad_rename", "need session_id or dir")
			return
		}
		var err error
		if rec, err = c.srv.registerDiscovered(sessionID, dir); err != nil {
			c.fail("internal", err.Error())
			return
		}
	}
	c.doRename(rec.Name, newName)
	c.doDiscover()
}

// doSetAgent switches a session's AI backend (and model) durably. It mirrors
// doRenameDiscovered: it locates the session by session_id (registering a still-
// discovered one by dir first), then stamps the resolved backend + a model valid
// for it (an explicit alias that isn't in the new backend's catalogue falls back to
// its default). Changing the backend rotates to a fresh session_id and un-Starts
// the session: Claude and Codex transcripts use incompatible on-disk formats, so a
// switch begins a clean conversation on the new AI rather than trying to resume
// history it can't parse (the old transcript stays on disk, just off this chain).
func (c *conn) doSetAgent(sessionID, dir, agentID, modelAlias string) {
	rec := c.srv.store.GetBySessionID(sessionID)
	if rec == nil {
		if dir == "" {
			c.fail("bad_agent", "need session_id or dir")
			return
		}
		var err error
		if rec, err = c.srv.registerDiscovered(sessionID, dir); err != nil {
			c.fail("internal", err.Error())
			return
		}
	}
	attachedHere := c.attached != nil && c.attached.SessionID == rec.SessionID
	ag := c.srv.driver.Registry().Resolve(agentID)
	model := c.srv.driver.ProviderSettings().DefaultModel(ag)
	if modelAlias != "" {
		if m, ok := ag.Model(modelAlias); ok {
			model = m.Alias
		}
	}
	// Compare resolved ids so an empty/omitted agent (== the default backend) is a
	// no-op against a session already on that backend and doesn't force a rotation.
	curID := c.srv.driver.Registry().Resolve(rec.Agent).ID
	var oldID string
	if curID != ag.ID {
		if c.srv.isBusy(rec.SessionID) {
			c.fail("busy", "still working — switch the agent when the turn finishes")
			return
		}
		newID, err := session.NewSessionID()
		if err != nil {
			c.fail("internal", err.Error())
			return
		}
		oldID = rec.SessionID
		rec.SessionID = newID
		rec.Started = false
		rec.AskPrimed = false
		rec.JobsPrimed = false // re-prime the background-job instruction on the new backend
		rec.PriorIDs = nil     // don't chain the old backend's transcripts into the new one
		rec.PendingSeed = ""
	}
	rec.Agent = ag.ID
	rec.Model = model
	if err := c.srv.store.Put(rec); err != nil {
		c.fail("internal", err.Error())
		return
	}
	if oldID != "" {
		// The session_id rotated: move the hub + id index onto the new id so an
		// attached device still receives the next turn, and forget the old id.
		c.srv.rekeyJob(oldID, rec.SessionID)
		if ferr := c.srv.store.ForgetID(oldID); ferr != nil {
			log.Printf("forget rotated id %s: %v", oldID, ferr)
		}
	}
	if attachedHere {
		c.setAttached(rec)
		c.send(msgAttached(rec, nil)) // refresh the app's backend/model badge in place
	}
	c.sendSessionList()
	c.doDiscover()
}

// doAdopt registers a discovered Claude session (by session_id + dir) into the
// spawner store and attaches to it, so the app can drive/view it via --resume.
// If the session_id is already registered, it just attaches.
func (c *conn) doAdopt(sessionID, dir string) {
	if sessionID == "" || dir == "" {
		c.fail("bad_adopt", "adopt needs session_id and dir")
		return
	}
	if s := c.srv.store.GetBySessionID(sessionID); s != nil {
		c.doAttach(s.Name, false)
		return
	}
	// A session_id is the sole identity: adopt the requested one verbatim. A folder
	// that already hosts another session is fine — the adopted session is a distinct
	// one and its name simply dedups to "<dir>-2".
	rec, err := c.srv.registerDiscovered(sessionID, dir)
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.sendSessionList()
	c.doAttach(rec.Name, false)
}

// doDeleteDiscovered PERMANENTLY deletes a session row. A REGISTERED row is one
// session: its transcript(s) — current session_id plus rotated prior ids — and its
// single registry record; deleting it leaves dir-mates that have their own rows.
// An UNREGISTERED row stands for a whole directory of loose transcripts (discover
// shows one adoptable row per dir), so it wipes every transcript in that dir — else
// deleting one just re-surfaces the row on a sibling. Refuses while the directory is
// live in a terminal (deleting under a running claude corrupts it). Refreshes the
// discover + session lists.
func (c *conn) doDeleteDiscovered(sessionID string) {
	if sessionID == "" {
		c.fail("bad_delete", "need session_id")
		return
	}
	rec := c.srv.store.GetBySessionID(sessionID)
	var dir string
	if rec != nil {
		dir = rec.Dir
	} else if p := c.srv.driver.TranscriptPathByID("", sessionID); p != "" {
		dir = c.srv.driver.TranscriptCwd("", p)
	}
	if dir == "" {
		c.fail("not_found", "no transcript or record for that session")
		return
	}
	// Guard against corrupting a transcript that a live interactive claude in this
	// directory might be writing.
	if c.srv.tmuxMgr.ClaudeDirs(c.ctx)[dir] {
		c.fail("session_active", "that session is live in a terminal — close it there first")
		return
	}
	// A registered row is one session — remove exactly its transcripts (current id
	// plus rotated prior ids), leaving any dir-mates that have their own rows. An
	// unregistered row stands for the WHOLE directory (discover collapses a dir's
	// loose transcripts into a single adoptable row), so wipe every transcript in
	// that dir — otherwise deleting one just re-surfaces the row on a dir-mate.
	var err error
	if rec != nil {
		// Backend-aware full purge of the whole chain (current + rotated prior ids):
		// transcript, sidecar, and per-session state for Claude; rollout files for
		// Codex. Leaves any dir-mates that have their own rows.
		_, err = c.srv.driver.DeleteSession(rec.Agent, rec.Host, rec.TranscriptIDs())
	} else {
		// Unregistered rows come from the discovery scan on the loopback host
		// (TranscriptPathByID above reads the same place), so delete there.
		_, err = c.srv.driver.DeleteSessionsForDir(c.ctx, "", sessionID, dir)
	}
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	if rec != nil {
		if c.attached != nil && c.attached.SessionID == rec.SessionID {
			c.doDetach()
		}
		if derr := c.srv.store.Delete(rec.Name); derr != nil {
			log.Printf("delete session record %s: %v", rec.Name, derr)
		}
		c.removeSandbox(rec) // destroy the session's container, if any
		c.srv.dropJob(rec.SessionID)
	}
	c.sendSessionList()
	c.doDiscover()
}

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.
func (c *conn) serveHistory(name string, before *int, limit int, haveHash string) {
	s := c.srv.store.Get(name)
	if s == nil {
		c.fail("no_session", "no such session: "+name)
		return
	}
	msgs, err := c.srv.driver.ReadTranscriptChain(s.Agent, s.Host, s.TranscriptIDs())
	if err != nil {
		c.fail("history_failed", err.Error())
		return
	}
	count, hash := session.HistoryDigest(msgs)
	// Cache-validation fast path: a top-page request (before == nil) carrying the
	// hash the app already holds needs no message bodies — tell it the cache is
	// current so clicking back into an unchanged session transfers nothing.
	if before == nil && haveHash != "" && haveHash == hash {
		c.send(msgHistory(name, nil, false, count, hash, true))
		return
	}
	b := -1
	if before != nil {
		b = *before
	}
	page, more := session.HistoryPage(msgs, b, limit)
	// Strip the server-injected scaffolding from user messages so replayed history
	// matches what the live view showed (and never re-surfaces hidden instructions).
	for i := range page {
		if page[i].Role == "user" {
			page[i].Text = stripInjected(page[i].Text)
		}
	}
	c.send(msgHistory(name, page, more, count, hash, false))
}

// serveDigests reports every registered session's transcript digest (message
// count + content hash) so the app can validate its offline transcript cache on
// connect without transferring any message bodies — it refetches history only
// for sessions whose hash changed. Transcript reads are memoized by file stat,
// so recomputing digests when nothing changed is cheap. An unreadable session is
// skipped (the app keeps whatever it already cached for it).
func (c *conn) serveDigests() {
	sessions := c.srv.store.List()
	items := make([]digestView, 0, len(sessions))
	for _, s := range sessions {
		msgs, err := c.srv.driver.ReadTranscriptChain(s.Agent, s.Host, s.TranscriptIDs())
		if err != nil {
			continue
		}
		count, hash := session.HistoryDigest(msgs)
		items = append(items, digestView{Name: s.Name, SessionID: s.SessionID, Count: count, Hash: hash})
	}
	c.send(msgDigests(items))
}

// commandHelp is spoken + shown when the user asks "hey buddy help".
const commandHelp = "here's what I know: attach to a session, detach, list sessions, status, " +
	"kill a session, spawn a session, spawn a new project, read last, clear the context, compress the context, " +
	"list models, use model by number, stop the turn, cancel message, and help. " +
	"say hey buddy, then the command, then your end token."

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
func (c *conn) sendSessionList() {
	sessions := c.srv.store.List()
	views := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, sessionView{Name: s.Name, Dir: s.Dir, Target: sandboxTarget(s), Agent: s.Agent, Model: s.Model, Profile: s.Profile})
	}
	c.send(msgSessionList(views))
}

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
func (c *conn) selectClientSession(sessionID string) bool {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return true
	}
	if c.attached != nil && c.attached.SessionID == sessionID {
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
func (c *conn) doSetWhisperModel(name string, fast bool) {
	go func() {
		name = strings.TrimSpace(name)
		// Fetch the model first if it's a known catalog model not yet on disk; this
		// broadcasts download progress and is a no-op when it's already present.
		if err := c.srv.ensureModel(name, fast); err != nil {
			c.fail("whisper_failed", err.Error())
			return
		}
		if err := c.srv.setWhisperModel(name, fast); err != nil {
			c.fail("whisper_failed", err.Error())
			return
		}
		model, fastModel := c.srv.currentWhisperModels()
		// Persist the choice so a restart/rebuild keeps it instead of reverting to
		// the env default. A write failure is non-fatal — the live model is set.
		persist := c.srv.settings.SetWhisperModel(model)
		if fast {
			persist = c.srv.settings.SetWhisperFastModel(fastModel)
		}
		if persist != nil {
			log.Printf("settings: persist whisper model: %v", persist)
		}
		c.srv.broadcastWhisperModel()
	}()
}

// doRestart fires the configured restart command to rebuild and relaunch the
// server, picking up any new server code. The command (SPAWNER_RESTART_CMD) runs
// detached on the host — SSHing over and running deploy/rebuild-container.sh to
// rebuild the image and recreate this container — so the process is replaced out
// from under us and the app auto-reconnects once the fresh one is listening. Any authenticated client may
// trigger this; the trust boundary is the same as spawning arbitrary commands.
// Reports back if restart isn't configured (SPAWNER_RESTART_CMD unset) instead of
// pretending it worked.
// rebuild is nil for older clients / the voice command (treated as a full rebuild);
// an explicit false requests a fast bounce that recreates from the existing image
// without recompiling.
func (c *conn) doRestart(rebuild *bool) {
	full := rebuild == nil || *rebuild
	go func() {
		if err := c.srv.driver.Restart(context.Background(), full); err != nil {
			c.fail("restart_failed", err.Error())
			return
		}
		if full {
			c.srv.broadcast(msgSay("rebuilding and restarting the server — back in a moment."))
		} else {
			c.srv.broadcast(msgSay("restarting the server — back in a moment."))
		}
	}()
}

// abortTurn cancels the running turn on the attached session (kills the claude
// child). The turn's goroutine then delivers a `turn_stopped` to clear the app.
func (c *conn) abortTurn() {
	if c.attached != nil && c.srv.cancelTurn(c.attached.SessionID) {
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
func (c *conn) doSwap() {
	if c.prevSessionID == "" {
		c.send(msgSay("no previous session to swap to."))
		return
	}
	prev := c.srv.store.GetBySessionID(c.prevSessionID)
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
	c.send(msgContextReset(s.Name)) // reset the app's context-size readout to zero
	c.send(msgSay("cleared. starting fresh — your history is still here."))
}

// doListModels speaks the models the attached session's AI backend offers, in
// order, so the user can pick one by NUMBER ("use model 2"). Ordinal selection
// keeps hard-to-say model names (e.g. Codex's gpt-5.5 reasoning presets) out of
// the voice path. Marks the session's current model.
func (c *conn) doListModels() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ag := c.srv.driver.AgentFor(c.attached)
	// Only the voice-enumerable subset (per the Providers settings) is spoken and
	// numbered, so hard-to-say or hidden models stay out of the spoken flow. The
	// ordinals here must match doUseModel, which indexes the same subset.
	models := c.srv.driver.ProviderSettings().VoiceModels(ag)
	if ag == nil || len(models) == 0 {
		c.send(msgSay("this session's AI has no selectable models."))
		return
	}
	// An empty session Model means the backend's own default — mark that one.
	current := c.attached.Model
	if current == "" {
		current = c.srv.driver.ProviderSettings().DefaultModel(ag)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d models. ", ag.Name, len(models))
	for i, m := range models {
		mark := ""
		if m.Alias == current {
			mark = ", current"
		}
		fmt.Fprintf(&b, "%d, %s%s. ", i+1, m.Alias, mark)
	}
	b.WriteString("say use model and a number to switch.")
	c.send(msgSay(b.String()))
}

// doUseModel switches the attached session's model to the n-th in its backend's
// catalogue (1-based, matching doListModels). Durable — persisted on the session
// and read by the next turn (a turn already in flight finishes on the old model).
func (c *conn) doUseModel(n int) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ag := c.srv.driver.AgentFor(c.attached)
	models := c.srv.driver.ProviderSettings().VoiceModels(ag)
	if ag == nil || len(models) == 0 {
		c.send(msgSay("this session's AI has no selectable models."))
		return
	}
	if n < 1 || n > len(models) {
		c.send(msgSay(fmt.Sprintf("say a model number between 1 and %d — list models to hear them.", len(models))))
		return
	}
	m := models[n-1]
	c.attached.Model = m.Alias
	if err := c.srv.store.Put(c.attached); err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.send(msgSay(fmt.Sprintf("switched to %s. it takes effect on your next message.", m.Alias)))
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
	if c.srv.isBusy(s.SessionID) {
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
	// state) for its backend — not just the registry record — so nothing about it
	// is left on disk. Best-effort: a purge error still drops the record below.
	if _, err := c.srv.driver.DeleteSession(s.Agent, s.Host, s.TranscriptIDs()); err != nil {
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
		c.fail("bad_message", "need a session name")
		return
	}
	c.removeSession(name)
}

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
	// Follow the rename if we're attached to it. The job hub and in-flight state are
	// keyed by session_id (stable across a rename), so nothing there needs re-keying;
	// just refresh the attached record and update the app's title in place.
	if c.attached != nil && c.attached.Name == old {
		c.setAttached(c.srv.store.Get(newName))
		c.send(msgRenamed(old, newName, c.attached.SessionID)) // update the attached-session title in place (matched by id)
	}
	c.sendSessionList() // push the refreshed list back to the app (quietly)
	return true
}

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
func (c *conn) dictate(text string) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	// Reconcile detached background jobs at the turn boundary (before the prompt is
	// built) so a job that finished since the last turn gets its completion note
	// staged into PendingNotes now. Safe here: no turn is in flight yet, so this
	// doesn't race the running turn's own store.Put (the one-writer invariant).
	c.srv.reconcileJobs(c.attached)
	prompt := text
	// A prior "compress" left a compacted summary of the old context to carry into
	// this fresh session_id; prepend it to the FIRST dictation so Claude continues
	// with that condensed context. startTurn clears PendingSeed once the turn lands.
	if c.attached.PendingSeed != "" && !c.attached.Started {
		prompt = seedPreamble(c.attached.PendingSeed) + prompt
	}
	// Prepend any framed background-job completion notes so Claude learns a job it
	// started earlier has finished, then clear them (unconditionally — unlike the
	// compress seed, which is gated on !Started). stripInjected strips this back off
	// stored history so the echoed view stays clean.
	if len(c.attached.PendingNotes) > 0 {
		prompt = jobNotesPreamble(c.attached.PendingNotes) + prompt
		c.attached.PendingNotes = nil
		if err := c.srv.store.Put(c.attached); err != nil {
			log.Printf("dictate[%s]: persist cleared notes: %v", c.attached.Name, err)
		}
	}
	if c.brief {
		// Opt-in: nudge Claude toward short, TTS-friendly replies. Only the prompt
		// to Claude carries the hint; the displayed/echoed transcript stays as spoken.
		prompt += briefSuffix
	}
	// Interactive mode: append the ask instruction only until it's been primed for
	// this context. Claude retains it across turns via --resume, so re-sending it
	// every turn just burns tokens; a `clear` resets AskPrimed to re-prime.
	primeAsk := c.interactive && !c.attached.AskPrimed
	if primeAsk {
		prompt += askInstruction // let Claude ask instead of guessing (parsed back on reply)
	}
	// Prime the background-job instruction once per context (like AskPrimed): tell
	// Claude to route long-running commands through spawner-job instead of
	// run_in_background, so they survive turns. Claude retains it via --resume;
	// clear/compress reset JobsPrimed to re-prime after a rotation (harmless).
	primeJobs := !c.attached.JobsPrimed
	if primeJobs {
		prompt += jobsInstruction(session.JobScriptPath(session.HostHome()))
	}
	if !c.srv.startTurn(c.attached, prompt, primeAsk, primeJobs) {
		c.send(msgSay("still working on the last one."))
		return
	}
	// Mirror the prompt onto any other devices attached to this session.
	c.srv.echoUserPrompt(c.attached.SessionID, text, c)
}

// Scaffolding the server appends to a dictation before sending it to Claude. It's
// deliberately kept out of the live echo (dictate sends the raw text to other
// devices), so history — read back from Claude's transcript, which stores the
// augmented prompt — must strip it too (stripInjected) to match the live view.
const briefSuffix = "\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)"

// seedPreamble frames a compress summary as leading context ahead of the user's
// first dictation on the rotated session, so Claude treats it as the recap of the
// prior conversation rather than as a new instruction.
const (
	seedRecapOpen  = "[Continuing from a compacted session — recap of the conversation so far:]\n\n"
	seedRecapClose = "\n\n[End of recap. The user's message follows.]\n\n"
)

func seedPreamble(seed string) string {
	return seedRecapOpen + seed + seedRecapClose
}

// stripInjected removes the server-appended prompt scaffolding — the brief-reply
// nudge, the interactive-mode ask instruction, and any compress recap preamble —
// from a stored user message, so history shows exactly the text the user spoke.
// This keeps the history view consistent with the live echo (which never carried
// the scaffolding) and lets the app dedupe a replayed turn against its live copy.
func stripInjected(text string) string {
	// The background-job instruction is a suffix like askInstruction but carries a
	// dynamic script path, so strip from its marker to the end rather than by exact
	// match. Do this before the fixed-suffix trims (it may sit after them).
	if i := strings.Index(text, jobsInstructionMark); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSuffix(text, askInstruction)
	text = strings.TrimSuffix(text, briefSuffix)
	// Job-completion notes are prepended (parallel to the seed recap); strip that
	// framed block back off stored history.
	if strings.HasPrefix(text, jobNotesOpen) {
		if i := strings.Index(text, jobNotesClose); i >= 0 {
			text = text[i+len(jobNotesClose):]
		}
	}
	if strings.HasPrefix(text, seedRecapOpen) {
		if i := strings.Index(text, seedRecapClose); i >= 0 {
			text = text[i+len(seedRecapClose):]
		}
	}
	return text
}

// jobsInstructionMark is the leading, path-free marker of jobsInstruction, used by
// stripInjected to remove the whole (dynamic-path) instruction from stored history.
const jobsInstructionMark = "\n\n[Background jobs] For any command that should keep running"

// affirmative / negative recognize yes/no style dialog replies. `extra` carries
// the connection's custom wake token so "<wake> yes" strips like "hey buddy yes".
func affirmative(text string, extra [][]string) bool {
	r, _ := command.StripWakeWith(text, extra)
	return command.Parse(r).Kind != command.Cancel &&
		containsAny(r, "yes", "yeah", "yep", "yup", "sure", "do it", "please", "go ahead", "ok", "okay")
}

func negative(text string, extra [][]string) bool {
	r, _ := command.StripWakeWith(text, extra)
	return containsAny(r, "no", "nope", "nah", "don't", "do not", "scrap", "skip")
}

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
func (c *conn) ensureSandbox(s *session.Session) {
	if err := c.srv.driver.EnsureContainer(c.ctx, s); err != nil {
		log.Printf("sandbox ensure for %s: %v", s.Name, err)
	}
}

// removeSandbox best-effort destroys a session's persistent container on delete.
// Logged, never fatal — a runtime hiccup must not block removing the record.
func (c *conn) removeSandbox(s *session.Session) {
	if err := c.srv.driver.RemoveContainer(c.ctx, s); err != nil {
		log.Printf("sandbox remove for %s: %v", s.Name, err)
	}
}

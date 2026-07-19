package gateway

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
)

// dialog is the state for the spawn flow: ask for a full spoken path, resolve it
// against the target host's real filesystem, then confirm the execution target
// and whether to attach.
type dialog struct {
	flow  string // "spawn"
	state string // await_path | await_target | await_attach
	mode  string // "session" (spawn in an existing dir) | "new" (create the final folder)
	dir   string // absolute dir once known (for create/attach)
	// attachPrompt is the pending "want to attach?" line, stashed across the
	// optional host-vs-sandbox target question so the flow resumes cleanly.
	attachPrompt string
	sess         *session.Session
	// agentID is the AI backend chosen inline in the spawn phrase ("spawn a codex
	// session"); empty means the default backend. Carried across the dialog to
	// newSession so the created session runs on the chosen backend.
	agentID string
}

// pathResult is how far resolveSpokenPath got interpreting a spoken absolute path.
type pathResult int

const (
	pathOK        pathResult = iota // every segment resolved to a real directory
	pathAmbiguous                   // a segment fuzzy-matched several children with no clear winner
	pathMissing                     // a segment matched no child (typo, or a to-be-created folder)
)

// spawnCommand handles a voice "spawn"/"new session" utterance. It takes the FAST
// path — resolve the spoken absolute path against the target host's real
// filesystem and spawn immediately, filling in the default provider/profile for
// anything unnamed — whenever the path resolves cleanly. It falls back to the
// interactive dialog (which asks for, or reconfirms, the full path) for a new
// *project*, a bare command with no path, or a path that's ambiguous or doesn't
// resolve. There is no SPAWNER_ROOT jail: a session may spawn anywhere on the
// target, and the directory is only its initial working dir.
func (c *conn) spawnCommand(intent command.Intent) {
	if intent.New {
		c.startSpawn(intent.New, intent.Location, intent.Agent)
		return
	}
	segs, _ := parseSpokenPath(intent.Location)
	if len(segs) == 0 {
		c.startSpawn(false, intent.Location, intent.Agent) // no path spoken → ask for one
		return
	}
	dir, _, res := c.resolveSpokenPath(session.LocalHost, segs)
	if res != pathOK {
		c.startSpawn(false, intent.Location, intent.Agent) // ambiguous / not found → reprompt
		return
	}
	profileID := c.resolveProfileName(intent.Profile)
	name := sanitizeName(strings.Join(projects.Terms(intent.Name), "-"))
	c.doSpawnAt(dir, "", false, session.LocalHost, intent.Agent, "", profileID, name, true)
}

// parseSpokenPath turns a spoken absolute path into its segments. The wake-word
// transcript renders a path separator as either a literal "/" or the spoken word
// "slash" (Whisper does both), so we treat both as segment boundaries; each
// segment keeps its remaining spoken words ("claude spawner") for fuzzy matching.
// absolute reports whether a leading separator was present. Examples:
//
//	"slash home slash bam slash git" -> [home bam git], absolute
//	"/home/bam/git"                  -> [home bam git], absolute
func parseSpokenPath(raw string) (segs []string, absolute bool) {
	fields := strings.Fields(strings.ToLower(raw))
	for i, f := range fields {
		if f == "slash" || f == "backslash" {
			fields[i] = "/"
		}
	}
	parts := strings.Split(strings.Join(fields, " "), "/")
	absolute = len(parts) > 0 && strings.TrimSpace(parts[0]) == ""
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			segs = append(segs, p)
		}
	}
	return segs, absolute
}

// resolveSpokenPath walks segs down from the filesystem root on host, listing the
// REAL directories at each level (over SSH — the target machine, not the server's
// container) and fuzzy-matching each spoken segment against them, so a mis-heard
// component is auto-corrected to the closest actually-existing child ("colmb" ->
// "home"). It returns the resolved directory, any segments left unresolved, and
// the outcome: pathOK when every segment landed on a clear directory, pathAmbiguous
// when a segment matched several children with no clear winner, pathMissing when a
// segment matched nothing (a typo, or — for new-project mode — a folder to create).
func (c *conn) resolveSpokenPath(host string, segs []string) (dir string, unresolved []string, res pathResult) {
	dir = "/"
	for i, seg := range segs {
		entries, err := c.listDir(host, dir, false)
		if err != nil {
			return dir, segs[i:], pathMissing
		}
		dirs := make([]projects.Dir, 0, len(entries))
		for _, e := range entries {
			dirs = append(dirs, projects.Dir{Name: e.Name, Path: e.Path})
		}
		ranked := projects.Rank(seg, dirs)
		switch {
		case len(ranked) == 0:
			return dir, segs[i:], pathMissing
		case len(ranked) > 1:
			return dir, segs[i:], pathAmbiguous // no clear winner — make the user disambiguate
		default:
			dir = ranked[0].Path
		}
	}
	return dir, nil, pathOK
}

// resolveProfileName maps a spoken profile term to a registered profile name,
// tolerating spacing and case ("bare metal" -> "bare-metal"). An empty term or no
// match returns "" — the spawn then uses the default profile.
func (c *conn) resolveProfileName(term string) string {
	if strings.TrimSpace(term) == "" {
		return ""
	}
	want := sanitizeName(strings.Join(projects.Terms(term), "-"))
	profiles := c.srv.driver.ProfileRegistry().List()
	for _, p := range profiles {
		if strings.EqualFold(p.Name, want) {
			return p.Name
		}
	}
	for _, p := range profiles { // loose match, e.g. "sand" -> "sandbox"
		if strings.Contains(strings.ToLower(p.Name), want) {
			return p.Name
		}
	}
	return ""
}

// startSpawn begins the spawn dialog. isNew selects create-a-new-project mode
// (the last path segment names a folder to create); location is an optional spoken
// absolute path to jump straight to; agentID is the backend chosen inline ("spawn a
// codex session"), empty = default.
func (c *conn) startSpawn(isNew bool, location, agentID string) {
	mode := "session"
	if isNew {
		mode = "new"
	}
	c.dlg = &dialog{flow: "spawn", state: "await_path", mode: mode, agentID: agentID}
	if segs, _ := parseSpokenPath(location); len(segs) > 0 {
		c.resolveAndArrive(segs) // named inline — try it, reprompt if it doesn't land
		return
	}
	c.promptPath()
}

// promptPath asks for the full spoken path to spawn in.
func (c *conn) promptPath() {
	what := "the full path"
	if c.dlg.mode == "new" {
		what = "the full path, ending in the new folder's name"
	}
	c.send(msgDialog("await_path", "where to?"))
	c.send(msgSay("where to? say " + what + ", like slash home slash bam slash git."))
}

// repromptPath re-asks for the path after an unresolved or ambiguous attempt.
func (c *conn) repromptPath(reason string) {
	c.send(msgDialog("await_path", "where to?"))
	c.send(msgSay(reason + " — say the full path again, like slash home slash bam slash git."))
}

// spawnAwaitPath handles a spoken path in the dialog.
func (c *conn) spawnAwaitPath(text string) {
	segs, _ := parseSpokenPath(text)
	if len(segs) == 0 {
		c.repromptPath("i didn't catch a path")
		return
	}
	c.resolveAndArrive(segs)
}

// resolveAndArrive resolves the spoken path segments against the target host and
// either spawns there (session mode) or creates the final folder first (new mode),
// reprompting when the path is ambiguous or can't be placed.
func (c *conn) resolveAndArrive(segs []string) {
	host := session.LocalHost
	if c.dlg.mode == "new" {
		// The last segment names the folder to create; resolve its parent path.
		parentSegs, newName := segs[:len(segs)-1], sanitizeName(segs[len(segs)-1])
		if newName == "" {
			c.repromptPath("i need a name for the new folder")
			return
		}
		parent, _, res := c.resolveSpokenPath(host, parentSegs)
		if res != pathOK {
			c.repromptPath("i couldn't place that folder")
			return
		}
		dir := filepath.Join(parent, newName)
		if err := c.srv.driver.MakeSpawnDir(c.ctx, dir); err != nil {
			c.fail("spawn_failed", err.Error())
			c.dlg = nil
			return
		}
		c.askTarget(dir, "created "+newName+". want to attach?")
		return
	}
	dir, _, res := c.resolveSpokenPath(host, segs)
	if res != pathOK {
		c.repromptPath("i couldn't place that path")
		return
	}
	c.askTarget(dir, "found "+filepath.Base(dir)+". want to attach?")
}

// repromptDialog re-issues the current dialog's prompt (used when resuming a
// dialog after a reconnect).
func (c *conn) repromptDialog() {
	switch c.dlg.state {
	case "await_path":
		c.promptPath()
	case "await_target":
		c.send(msgDialog("await_target", "host or sandbox?"))
		c.send(msgSay("run " + filepath.Base(c.dlg.dir) + " on the host, or in a sandbox?"))
	case "await_attach":
		c.send(msgDialog("await_attach", "want to attach?"))
		if c.dlg.sess != nil {
			c.send(msgSay("want to attach to " + c.dlg.sess.Name + "?"))
		}
	}
}

// cancelDialog aborts any in-progress dialog.
func (c *conn) cancelDialog() {
	if c.dlg == nil {
		return
	}
	c.dlg = nil
	c.send(msgSay("ok, cancelled."))
}

// handleDialog advances the active dialog with the user's latest utterance.
func (c *conn) handleDialog(text string) {
	// "cancel"/"never mind" aborts from any state.
	if rest, _ := c.stripWake(text); command.Parse(rest).Kind == command.Cancel {
		c.cancelDialog()
		return
	}
	switch c.dlg.state {
	case "await_path":
		c.spawnAwaitPath(text)
	case "await_target":
		c.spawnAwaitTarget(text)
	case "await_attach":
		c.spawnAwaitAttach(text)
	default:
		c.dlg = nil
		c.fail("internal", "unknown dialog state")
	}
}

// askTarget decides where a new session at dir will run. With no sandbox
// configured every session runs on the host, so we skip straight to the attach
// question; otherwise we ask host-vs-sandbox first and the answer becomes
// Session.Target. attachPrompt is the "want to attach?" line to use afterward.
func (c *conn) askTarget(dir, attachPrompt string) {
	if !c.srv.driver.SandboxEnabled() {
		c.beginAttachQuestion(dir, attachPrompt, session.TargetHost)
		return
	}
	c.dlg.dir = dir
	c.dlg.attachPrompt = attachPrompt
	c.dlg.state = "await_target"
	c.send(msgDialog("await_target", "host or sandbox?"))
	c.send(msgSay("run " + filepath.Base(dir) + " on the host, or in a sandbox?"))
}

// spawnAwaitTarget records the chosen execution target and continues to the
// attach question.
func (c *conn) spawnAwaitTarget(text string) {
	switch {
	case containsAny(text, "sandbox", "sandboxed", "container", "isolated"):
		c.beginAttachQuestion(c.dlg.dir, c.dlg.attachPrompt, session.TargetSandbox)
	case containsAny(text, "host", "directly", "normal", "local", "machine", "here"):
		c.beginAttachQuestion(c.dlg.dir, c.dlg.attachPrompt, session.TargetHost)
	default:
		c.send(msgSay("host or sandbox?"))
	}
}

// beginAttachQuestion prepares a session record for the chosen dir (in memory)
// and moves to the attach question. The record is only persisted once the user
// answers, so "cancel" leaves no junk behind.
func (c *conn) beginAttachQuestion(dir, prompt string, target session.Target) {
	// A directory is just the session's initial working dir, not its identity — so
	// this always mints a NEW session, even when the folder already hosts one (the
	// name dedups to "<dir>-2" via newSession's uniqueName). Re-attaching to an
	// existing session is the attach flow's job, not re-spawning in its folder.
	sess, err := c.newSession(sanitizeName(filepath.Base(dir)), dir, target, c.dlg.agentID, "")
	if err != nil {
		c.fail("internal", err.Error())
		c.dlg = nil
		return
	}
	c.dlg.sess = sess
	c.dlg.dir = dir
	c.dlg.state = "await_attach"
	c.send(msgDialog("await_attach", "want to attach?"))
	c.send(msgSay(prompt))
}

func (c *conn) spawnAwaitAttach(text string) {
	switch {
	case affirmative(text, c.wakePhrases()):
		sess := c.dlg.sess
		if perr := c.srv.store.Put(sess); perr != nil {
			c.fail("internal", perr.Error())
			c.dlg = nil
			return
		}
		c.ensureSandbox(sess) // start the persistent container for sandbox sessions
		c.dlg = nil
		if c.attached != nil {
			c.srv.unbindJob(c, c.attached.SessionID)
		}
		c.setAttached(sess)
		c.srv.bindJob(c, sess, true)   // register for live turn fan-out (fresh session: no catch-up)
		c.send(msgAttached(sess, nil)) // freshly spawned: no transcript, no context size yet
		where := "."
		if sess.Target == session.TargetSandbox {
			where = ", in a sandbox."
		}
		c.send(msgSay("attached to " + sess.Name + where))
	case negative(text, c.wakePhrases()):
		sess := c.dlg.sess
		if perr := c.srv.store.Put(sess); perr != nil {
			c.fail("internal", perr.Error())
			c.dlg = nil
			return
		}
		c.ensureSandbox(sess) // start the persistent container for sandbox sessions
		c.dlg = nil
		c.send(msgSay(sess.Name + " is ready when you are."))
	default:
		c.send(msgSay("attach? yes or no."))
	}
}

// uniqueName ensures the session name doesn't collide with an existing one.
func (s *Server) uniqueName(base string) string {
	if s.store.Get(base) == nil {
		return base
	}
	for i := 2; ; i++ {
		candidate := fmt.Sprintf("%s-%d", base, i)
		if s.store.Get(candidate) == nil {
			return candidate
		}
	}
}

// registerDiscovered creates and persists a store record for a discovered
// Claude session (session_id + dir) that isn't in the registry yet, giving it a
// unique auto-name from the directory basename. Shared by adopt / rename / fuzzy
// voice-attach, which all "adopt" an on-disk session the same way.
//
// It is IDEMPOTENT on the session_id (the sole identity): if this session_id is
// already registered it returns that record instead of adding a second. A folder
// that already hosts a DIFFERENT session is fine — that's a distinct session and
// this one's name simply dedups to "<dir>-2".
func (s *Server) registerDiscovered(sessionID, dir string) (*session.Session, error) {
	if existing := s.store.GetBySessionID(sessionID); existing != nil {
		return existing, nil
	}
	rec := &session.Session{
		Name:      s.uniqueName(sanitizeName(filepath.Base(dir))),
		Dir:       dir,
		SessionID: sessionID,
		Started:   true,
		// Discovery scans this machine's ~/.claude, so the session lives on the local
		// box: name it explicitly (LocalHost) rather than leaving Host empty, which
		// the SSH executor now rejects.
		Host: session.LocalHost,
	}
	if err := s.store.Put(rec); err != nil {
		return nil, err
	}
	return rec, nil
}

// sanitizeName keeps a session name to safe characters.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "session"
	}
	return out
}

func containsAny(t string, subs ...string) bool {
	t = strings.ToLower(t)
	for _, s := range subs {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// names extracts the display names from candidate directories.
func names(dirs []projects.Dir) []string {
	out := make([]string, len(dirs))
	for i, d := range dirs {
		out[i] = d.Name
	}
	return out
}

// orList renders ["a","b","c"] as "a, b, or c".
func orList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	default:
		return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
	}
}

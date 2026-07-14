package gateway

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
)

// maxList caps how many child folders we read aloud before just asking for a name.
const maxList = 8

// dialog is the state for the spawn flow: pick a root, then navigate down the
// directory tree, stopping at a project (git repo) or a plain folder.
type dialog struct {
	flow   string // "spawn"
	state  string // await_root | await_child | await_confirm | await_create | await_newname | await_target | await_attach
	mode   string // "session" (browse existing) | "new" (create a new project)
	dir    string // absolute dir once known (for create/attach)
	browse string // directory currently being navigated
	// attachPrompt is the pending "want to attach?" line, stashed across the
	// optional host-vs-sandbox target question so the flow resumes cleanly.
	attachPrompt string
	sess         *session.Session
	// agentID is the AI backend chosen inline in the spawn phrase ("spawn a codex
	// session"); empty means the default backend. Carried across the dialog to
	// newSession so the created session runs on the chosen backend.
	agentID string
}

// spawnCommand handles a voice "spawn"/"new session" utterance. It takes the
// FAST path — resolve a location (defaulting to the user's home directory), then
// create the session and attach right away, filling in the default provider and
// profile for anything the user didn't name — whenever it can pin a concrete
// directory. It falls back to the interactive browse dialog only when it can't:
// a new *project* (which needs a folder created + named), a spoken location that
// doesn't resolve or only fuzzily matches, or a location that lands on a root or
// namespace with child projects to choose among.
//
// The fast path only ever spawns in a directory derived from matchRoot/descend or
// the home default, all of which live under SPAWNER_ROOT — so the voice jail is
// preserved even though doSpawnAt itself isn't jailed.
func (c *conn) spawnCommand(intent command.Intent) {
	if intent.New {
		c.startSpawn(intent.New, intent.Location, intent.Agent)
		return
	}
	if c.srv.projects != nil {
		c.srv.projects.Refresh()
	}
	dir, ok := c.resolveSpawnDir(intent.Location)
	spoken := len(projects.Terms(intent.Location)) > 0
	// A *spoken* location that lands on a root or namespace with child projects is
	// ambiguous — drop into the browse dialog to choose among them. Naming the home
	// default explicitly ("in bam" when $HOME is /home/bam) is *not* ambiguous: it's
	// the same concrete target the bare no-location default spawns in, so honor it as
	// a one-shot instead of forcing a browse of home's children.
	namedHome := spoken && dir == c.homeSpawnDir()
	ambiguous := spoken && !namedHome && (c.isRoot(dir) || projects.IsNamespace(dir))
	// A no-location command normally keeps the classic "where do you want it?"
	// dialog. But if the user named anything else — a name, a provider, or a profile
	// — that's an explicit one-shot: honor the home default and spawn immediately,
	// even when home happens to be a configured spawn root (SPAWNER_ROOT contains
	// $HOME), which would otherwise trip the isRoot guard above.
	parameterized := len(projects.Terms(intent.Name)) > 0 ||
		strings.TrimSpace(intent.Agent) != "" || strings.TrimSpace(intent.Profile) != ""
	bareNoLocation := !spoken && !parameterized
	if !ok || ambiguous || bareNoLocation {
		c.startSpawn(intent.New, intent.Location, intent.Agent)
		return
	}
	profileID := c.resolveProfileName(intent.Profile)
	name := sanitizeName(strings.Join(projects.Terms(intent.Name), "-"))
	c.doSpawnAt(dir, "", false, session.LocalHost, intent.Agent, "", profileID, name, true)
}

// resolveSpawnDir turns a spoken location into a concrete directory. An empty
// location defaults to the user's home directory. A spoken location is matched to
// a root and descended; it returns ok=false when the root doesn't match or the
// descent only landed via a fuzzy stretch (so the caller falls back to the dialog,
// which confirms a misheard folder before committing).
func (c *conn) resolveSpawnDir(location string) (string, bool) {
	terms := projects.Terms(location)
	if len(terms) == 0 {
		return c.homeSpawnDir(), true
	}
	root := c.matchRoot(terms[0])
	if root == "" {
		return "", false
	}
	dir, inexact := c.descend(root, terms[1:])
	if inexact {
		return "", false
	}
	return dir, true
}

// homeSpawnDir is the default spawn location when none is spoken: the user's home
// directory, but only if it's inside a SPAWNER_ROOT (keeping the voice jail); else
// the first configured root.
func (c *conn) homeSpawnDir() string {
	home := os.Getenv("HOME")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home != "" && c.underRoots(home) {
		return home
	}
	if len(c.srv.cfg.SpawnRoots) > 0 {
		return c.srv.cfg.SpawnRoots[0]
	}
	return home
}

// underRoots reports whether dir is one of the configured spawn roots or lives
// beneath one.
func (c *conn) underRoots(dir string) bool {
	for _, r := range c.srv.cfg.SpawnRoots {
		if dir == r || strings.HasPrefix(dir, r+string(filepath.Separator)) {
			return true
		}
	}
	return false
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

// startSpawn begins the spawn dialog. isNew selects create-a-new-project mode;
// location is an optional spoken path ("git personal") to jump straight to;
// agentID is the backend chosen inline ("spawn a codex session"), empty = default.
func (c *conn) startSpawn(isNew bool, location, agentID string) {
	if c.srv.projects != nil {
		c.srv.projects.Refresh()
	}
	mode := "session"
	if isNew {
		mode = "new"
	}
	c.dlg = &dialog{flow: "spawn", state: "await_root", mode: mode, agentID: agentID}

	// If they named a location inline, resolve it and skip the "where?" prompt.
	if terms := projects.Terms(location); len(terms) > 0 {
		if root := c.matchRoot(terms[0]); root != "" {
			dir, inexact := c.descend(root, terms[1:])
			c.arriveAt(dir, inexact)
			return
		}
	}

	roots := c.rootNames()
	example := ""
	if len(roots) > 0 {
		verb := "personal"
		if isNew {
			verb = "personal, then a name"
		}
		example = " — then a folder, like '" + roots[0] + " " + verb + "'"
	}
	c.send(msgDialog("await_root", "where to?"))
	c.send(msgSay("where to? say " + orList(roots) + example + "."))
}

// arriveAt handles landing on a directory: in "new" mode ask for the new
// project's name; otherwise browse it (list/pick existing).
func (c *conn) arriveAt(dir string, inexact bool) {
	if c.dlg.mode == "new" {
		// New-project mode names the folder aloud ("...called, in mail_play?"),
		// so a fuzzy landing is already surfaced — no separate confirm needed.
		c.promptNewName(dir)
		return
	}
	c.browseInto(dir, inexact)
}

// promptNewName asks what to call a new project created under dir.
func (c *conn) promptNewName(dir string) {
	c.dlg.browse = dir
	c.dlg.state = "await_newname"
	c.send(msgDialog("await_newname", "new project name?"))
	c.send(msgSay("what's the new project called, in " + filepath.Base(dir) + "?"))
}

// spawnAwaitNewName creates the named directory under the chosen location.
func (c *conn) spawnAwaitNewName(text string) {
	name := sanitizeName(strings.Join(projects.Terms(text), "-"))
	if name == "" {
		c.send(msgSay("what should I call it?"))
		return
	}
	dir := filepath.Join(c.dlg.browse, name)
	if err := c.srv.driver.MakeSpawnDir(c.ctx, dir); err != nil {
		c.fail("spawn_failed", err.Error())
		c.dlg = nil
		return
	}
	c.askTarget(dir, "created "+name+". want to attach?")
}

// repromptDialog re-issues the current dialog's prompt (used when resuming a
// dialog after a reconnect).
func (c *conn) repromptDialog() {
	switch c.dlg.state {
	case "await_root":
		c.send(msgDialog("await_root", "where to?"))
		c.send(msgSay("where to? say " + orList(c.rootNames()) + "."))
	case "await_child":
		c.send(msgDialog("await_child", "which folder?"))
		c.promptChildren(c.dlg.browse)
	case "await_confirm":
		c.send(msgDialog("await_confirm", "did you mean "+filepath.Base(c.dlg.dir)+"?"))
		c.send(msgSay("did you mean " + filepath.Base(c.dlg.dir) + "? yes or no."))
	case "await_newname":
		c.send(msgDialog("await_newname", "new project name?"))
		c.send(msgSay("what's the new project called, in " + filepath.Base(c.dlg.browse) + "?"))
	case "await_create":
		c.send(msgDialog("await_create", "create it?"))
		c.send(msgSay("create " + filepath.Base(c.dlg.dir) + "? yes or no."))
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
	case "await_root":
		c.spawnAwaitRoot(text)
	case "await_child":
		c.spawnAwaitChild(text)
	case "await_confirm":
		c.spawnAwaitConfirm(text)
	case "await_create":
		c.spawnAwaitCreate(text)
	case "await_newname":
		c.spawnAwaitNewName(text)
	case "await_target":
		c.spawnAwaitTarget(text)
	case "await_attach":
		c.spawnAwaitAttach(text)
	default:
		c.dlg = nil
		c.fail("internal", "unknown dialog state")
	}
}

// spawnAwaitRoot picks a root ("data"/"git") and descends through any extra
// segments the user spoke ("git personal askii").
func (c *conn) spawnAwaitRoot(text string) {
	terms := projects.Terms(text)
	if len(terms) == 0 {
		c.send(msgSay("say " + orList(c.rootNames()) + "."))
		return
	}
	root := c.matchRoot(terms[0])
	if root == "" {
		c.send(msgSay("start with " + orList(c.rootNames()) + "."))
		return
	}
	dir, inexact := c.descend(root, terms[1:])
	c.arriveAt(dir, inexact)
}

// spawnAwaitChild navigates within the current browse directory: descend into a
// named subfolder, use the current one ("here"), read the folders ("list"), or
// offer to create a new one when nothing matches.
func (c *conn) spawnAwaitChild(text string) {
	switch {
	case containsAny(text, "here", "this one", "this folder", "use this", "use it", "current"):
		c.askTarget(c.dlg.browse, "ok, "+filepath.Base(c.dlg.browse)+". want to attach?")
		return
	case containsAny(text, "list", "what's in", "what folders", "read them"):
		all, recent := listMode(text)
		c.readChildren(c.dlg.browse, all, recent)
		return
	}

	terms := projects.Terms(text)
	if len(terms) == 0 {
		c.send(msgSay("say a folder in " + filepath.Base(c.dlg.browse) + ", or 'here'."))
		return
	}
	if dir, inexact := c.descend(c.dlg.browse, terms); dir != c.dlg.browse {
		c.browseInto(dir, inexact)
		return
	}

	// Nothing matched — offer to create a new folder named after what was said.
	name := sanitizeName(strings.Join(terms, "-"))
	c.dlg.dir = filepath.Join(c.dlg.browse, name)
	c.dlg.state = "await_create"
	c.send(msgDialog("await_create", "create it?"))
	c.send(msgSay("no folder like that in " + filepath.Base(c.dlg.browse) + ". create " + name + "? yes or no."))
}

// browseInto prompts for a subfolder when dir is a root or a namespace (a
// container of repos), otherwise treats dir as the target project.
func (c *conn) browseInto(dir string, inexact bool) {
	if c.isRoot(dir) || projects.IsNamespace(dir) {
		c.dlg.browse = dir
		c.dlg.state = "await_child"
		c.send(msgDialog("await_child", "which folder?"))
		c.promptChildren(dir)
		return
	}
	// A leaf project — we'd commit to it next. If we only got here by a fuzzy
	// match, confirm the folder name before proceeding to attach.
	if inexact {
		c.confirmMatch(dir)
		return
	}
	c.askTarget(dir, "found "+filepath.Base(dir)+". want to attach?")
}

// confirmMatch asks the user to confirm a fuzzy-matched folder before committing
// to it ("did you mean mail_play?"), so a misheard name doesn't silently attach
// to the wrong project.
func (c *conn) confirmMatch(dir string) {
	c.dlg.dir = dir
	c.dlg.state = "await_confirm"
	c.send(msgDialog("await_confirm", "did you mean "+filepath.Base(dir)+"?"))
	c.send(msgSay("i don't see that exactly — did you mean " + filepath.Base(dir) + "? yes or no."))
}

// spawnAwaitConfirm handles the yes/no after a fuzzy-match confirmation.
func (c *conn) spawnAwaitConfirm(text string) {
	switch {
	case affirmative(text, c.wakePhrase):
		c.askTarget(c.dlg.dir, "found "+filepath.Base(c.dlg.dir)+". want to attach?")
	case negative(text, c.wakePhrase):
		// Back up to the parent so they can pick again.
		parent := filepath.Dir(c.dlg.dir)
		c.dlg.browse = parent
		c.dlg.state = "await_child"
		c.send(msgDialog("await_child", "which folder?"))
		c.promptChildren(parent)
	default:
		c.send(msgSay("yes or no — did you mean " + filepath.Base(c.dlg.dir) + "?"))
	}
}

// promptChildren asks which subfolder, reading them out if there aren't too many.
func (c *conn) promptChildren(dir string) {
	kids := projects.Children(dir)
	label := filepath.Base(dir)
	switch {
	case len(kids) == 0:
		c.askTarget(dir, "nothing inside "+label+". use it anyway?")
	case len(kids) <= maxList:
		c.send(msgSay("which folder in " + label + "? " + orList(names(kids)) + "."))
	default:
		c.send(msgSay(label + " has a lot of folders. say a name, or 'list', 'list all', or 'list recent'."))
	}
}

// spawnAwaitCreate confirms creation of a brand-new directory.
func (c *conn) spawnAwaitCreate(text string) {
	switch {
	case affirmative(text, c.wakePhrase):
		if err := c.srv.driver.MakeSpawnDir(c.ctx, c.dlg.dir); err != nil {
			c.fail("spawn_failed", err.Error())
			c.dlg = nil
			return
		}
		c.askTarget(c.dlg.dir, "made it. want to attach?")
	case negative(text, c.wakePhrase):
		c.cancelDialog()
	default:
		c.send(msgSay("yes or no — create it?"))
	}
}

// descend walks `segs` down from dir, taking the best-matching child per
// segment; it stops at the first segment that matches nothing.
func (c *conn) descend(dir string, segs []string) (string, bool) {
	start := dir
	for _, seg := range segs {
		ranked := projects.Rank(seg, projects.Children(dir))
		if len(ranked) == 0 {
			break
		}
		dir = ranked[0].Path
	}
	// Flag a fuzzy landing: the matcher stretched to a folder whose name carries
	// a token the user never said ("mail" -> "mail_play"). A multi-word folder
	// the user actually named in full ("mail play" -> "mail_play") is exact.
	inexact := dir != start && !landedExact(filepath.Base(dir), segs)
	return dir, inexact
}

// landedExact reports whether every token in the destination folder's name was
// actually spoken (exactly or via a fuzzy transcription slip). A folder with an
// unspoken token means the matcher stretched to a longer name than was said.
func landedExact(name string, spoken []string) bool {
	for _, tok := range projects.Terms(name) {
		matched := false
		for _, s := range spoken {
			if strings.EqualFold(s, tok) || projects.FuzzyEqual(s, tok) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

// listMode parses "list" qualifiers: "all"/"alphabetical" list everything,
// "recent" sorts by modification time (newest first). Plain "list" shows a
// short alphabetical sample.
func listMode(text string) (all, recent bool) {
	t := strings.ToLower(text)
	recent = containsAny(t, "recent", "recently", "newest", "latest", "by date", "modified")
	alpha := containsAny(t, "alphabet", "by name", "sorted", "a to z")
	all = recent || alpha || containsAny(t, "all", "everything", "every")
	return
}

// readChildren reads out the child folders — a short sample by default, or all
// of them when `all`, ordered by recency when `recent`.
func (c *conn) readChildren(dir string, all, recent bool) {
	kids := projects.Children(dir) // alphabetical
	if len(kids) == 0 {
		c.send(msgSay("nothing in " + filepath.Base(dir) + "."))
		return
	}
	if recent {
		sort.SliceStable(kids, func(a, b int) bool {
			return modTime(kids[a].Path).After(modTime(kids[b].Path))
		})
	}
	limit := maxList
	if all {
		limit = len(kids)
	}
	if limit > len(kids) {
		limit = len(kids)
	}
	msg := orList(names(kids[:limit])) + "."
	if limit < len(kids) {
		msg += fmt.Sprintf(" and %d more — say 'list all'.", len(kids)-limit)
	}
	c.send(msgSay(msg))
}

// modTime returns the directory's modification time (zero on error).
func modTime(p string) time.Time {
	fi, err := os.Stat(p)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}

// rootNames returns the spoken labels for the roots (their basenames).
func (c *conn) rootNames() []string {
	var out []string
	for _, r := range c.srv.cfg.SpawnRoots {
		out = append(out, filepath.Base(r))
	}
	return out
}

// matchRoot resolves a spoken term to a root path, tolerating transcription
// slips ("get" for "git", "data" heard as "date") via edit distance. Returns ""
// if nothing is close enough.
func (c *conn) matchRoot(term string) string {
	term = strings.ToLower(term)
	best, bestDist := "", 1<<30
	for _, r := range c.srv.cfg.SpawnRoots {
		b := strings.ToLower(filepath.Base(r))
		if b == term || strings.Contains(b, term) || strings.Contains(term, b) {
			return r
		}
		if d := projects.Levenshtein(term, b); d < bestDist {
			best, bestDist = r, d
		}
	}
	if best != "" && bestDist <= 2 {
		return best
	}
	return ""
}

// isRoot reports whether dir is one of the configured roots.
func (c *conn) isRoot(dir string) bool {
	for _, r := range c.srv.cfg.SpawnRoots {
		if r == dir {
			return true
		}
	}
	return false
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
	// Reuse the directory's existing session rather than minting a same-folder
	// duplicate ("-2"); only create a fresh one when the folder has none.
	sess := c.srv.store.GetByDir(dir)
	if sess == nil {
		var err error
		sess, err = c.newSession(sanitizeName(filepath.Base(dir)), dir, target, c.dlg.agentID, "")
		if err != nil {
			c.fail("internal", err.Error())
			c.dlg = nil
			return
		}
	}
	c.dlg.sess = sess
	c.dlg.dir = dir
	c.dlg.state = "await_attach"
	c.send(msgDialog("await_attach", "want to attach?"))
	c.send(msgSay(prompt))
}

func (c *conn) spawnAwaitAttach(text string) {
	switch {
	case affirmative(text, c.wakePhrase):
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
	case negative(text, c.wakePhrase):
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
// It is IDEMPOTENT on the folder: the registry holds one local session per
// directory, so if this session_id — or ANY local (non-sandbox) session in the
// same dir — is already registered, it returns that existing record instead of
// adding a second. Without this, a stale discovered entry (e.g. a prior run's
// on-disk session_id for a folder that already has a live record) adopted via any
// caller would spawn a phantom "<dir>-2" duplicate. doAdopt guards this itself;
// centralizing it here covers the rename / set-agent / fuzzy-voice-attach callers
// too, so no path can create a per-folder duplicate.
func (s *Server) registerDiscovered(sessionID, dir string) (*session.Session, error) {
	if existing := s.store.GetBySessionID(sessionID); existing != nil {
		return existing, nil
	}
	if existing := s.store.GetByDirHost(dir, session.LocalHost); existing != nil {
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

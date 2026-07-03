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
	state  string // await_root | await_child | await_create | await_newname | await_attach
	mode   string // "session" (browse existing) | "new" (create a new project)
	dir    string // absolute dir once known (for create/attach)
	browse string // directory currently being navigated
	sess   *session.Session
}

// startSpawn begins the spawn dialog. isNew selects create-a-new-project mode;
// location is an optional spoken path ("git personal") to jump straight to.
func (c *conn) startSpawn(isNew bool, location string) {
	if c.srv.projects != nil {
		c.srv.projects.Refresh()
	}
	mode := "session"
	if isNew {
		mode = "new"
	}
	c.dlg = &dialog{flow: "spawn", state: "await_root", mode: mode}

	// If they named a location inline, resolve it and skip the "where?" prompt.
	if terms := projects.Terms(location); len(terms) > 0 {
		if root := c.matchRoot(terms[0]); root != "" {
			c.arriveAt(c.descend(root, terms[1:]))
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
	c.send(msgSay("where to, bud? say " + orList(roots) + example + "."))
}

// arriveAt handles landing on a directory: in "new" mode ask for the new
// project's name; otherwise browse it (list/pick existing).
func (c *conn) arriveAt(dir string) {
	if c.dlg.mode == "new" {
		c.promptNewName(dir)
		return
	}
	c.browseInto(dir)
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
		c.send(msgSay("what should I call it, bud?"))
		return
	}
	dir := filepath.Join(c.dlg.browse, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.send(msgError("spawn_failed", err.Error()))
		c.dlg = nil
		return
	}
	c.beginAttachQuestion(dir, "created "+name+". want to attach?")
}

// repromptDialog re-issues the current dialog's prompt (used when resuming a
// dialog after a reconnect).
func (c *conn) repromptDialog() {
	switch c.dlg.state {
	case "await_root":
		c.send(msgDialog("await_root", "where to?"))
		c.send(msgSay("where to, bud? say " + orList(c.rootNames()) + "."))
	case "await_child":
		c.send(msgDialog("await_child", "which folder?"))
		c.promptChildren(c.dlg.browse)
	case "await_newname":
		c.send(msgDialog("await_newname", "new project name?"))
		c.send(msgSay("what's the new project called, in " + filepath.Base(c.dlg.browse) + "?"))
	case "await_create":
		c.send(msgDialog("await_create", "create it?"))
		c.send(msgSay("create " + filepath.Base(c.dlg.dir) + "? yes or no."))
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
	if rest, _ := command.StripWake(text); command.Parse(rest).Kind == command.Cancel {
		c.cancelDialog()
		return
	}
	switch c.dlg.state {
	case "await_root":
		c.spawnAwaitRoot(text)
	case "await_child":
		c.spawnAwaitChild(text)
	case "await_create":
		c.spawnAwaitCreate(text)
	case "await_newname":
		c.spawnAwaitNewName(text)
	case "await_attach":
		c.spawnAwaitAttach(text)
	default:
		c.dlg = nil
		c.send(msgError("internal", "unknown dialog state"))
	}
}

// spawnAwaitRoot picks a root ("data"/"git") and descends through any extra
// segments the user spoke ("git personal askii").
func (c *conn) spawnAwaitRoot(text string) {
	terms := projects.Terms(text)
	if len(terms) == 0 {
		c.send(msgSay("say " + orList(c.rootNames()) + ", bud."))
		return
	}
	root := c.matchRoot(terms[0])
	if root == "" {
		c.send(msgSay("start with " + orList(c.rootNames()) + ", bud."))
		return
	}
	c.arriveAt(c.descend(root, terms[1:]))
}

// spawnAwaitChild navigates within the current browse directory: descend into a
// named subfolder, use the current one ("here"), read the folders ("list"), or
// offer to create a new one when nothing matches.
func (c *conn) spawnAwaitChild(text string) {
	switch {
	case containsAny(text, "here", "this one", "this folder", "use this", "use it", "current"):
		c.beginAttachQuestion(c.dlg.browse, "ok, "+filepath.Base(c.dlg.browse)+". want to attach?")
		return
	case containsAny(text, "list", "what's in", "what folders", "read them"):
		all, recent := listMode(text)
		c.readChildren(c.dlg.browse, all, recent)
		return
	}

	terms := projects.Terms(text)
	if len(terms) == 0 {
		c.send(msgSay("say a folder in " + filepath.Base(c.dlg.browse) + ", bud, or 'here'."))
		return
	}
	if dir := c.descend(c.dlg.browse, terms); dir != c.dlg.browse {
		c.browseInto(dir)
		return
	}

	// Nothing matched — offer to create a new folder named after what was said.
	name := sanitizeName(strings.Join(terms, "-"))
	c.dlg.dir = filepath.Join(c.dlg.browse, name)
	c.dlg.state = "await_create"
	c.send(msgDialog("await_create", "create it?"))
	c.send(msgSay("no folder like that in " + filepath.Base(c.dlg.browse) + ", bud. create " + name + "? yes or no."))
}

// browseInto prompts for a subfolder when dir is a root or a namespace (a
// container of repos), otherwise treats dir as the target project.
func (c *conn) browseInto(dir string) {
	if c.isRoot(dir) || projects.IsNamespace(dir) {
		c.dlg.browse = dir
		c.dlg.state = "await_child"
		c.send(msgDialog("await_child", "which folder?"))
		c.promptChildren(dir)
		return
	}
	c.beginAttachQuestion(dir, "found "+filepath.Base(dir)+". want to attach?")
}

// promptChildren asks which subfolder, reading them out if there aren't too many.
func (c *conn) promptChildren(dir string) {
	kids := projects.Children(dir)
	label := filepath.Base(dir)
	switch {
	case len(kids) == 0:
		c.beginAttachQuestion(dir, "nothing inside "+label+". use it anyway?")
	case len(kids) <= maxList:
		c.send(msgSay("which folder in " + label + "? " + orList(names(kids)) + "."))
	default:
		c.send(msgSay(label + " has a lot of folders, bud. say a name, or 'list', 'list all', or 'list recent'."))
	}
}

// spawnAwaitCreate confirms creation of a brand-new directory.
func (c *conn) spawnAwaitCreate(text string) {
	switch {
	case affirmative(text):
		if err := os.MkdirAll(c.dlg.dir, 0o755); err != nil {
			c.send(msgError("spawn_failed", err.Error()))
			c.dlg = nil
			return
		}
		c.beginAttachQuestion(c.dlg.dir, "made it. want to attach?")
	case negative(text):
		c.cancelDialog()
	default:
		c.send(msgSay("yes or no, bud — create it?"))
	}
}

// descend walks `segs` down from dir, taking the best-matching child per
// segment; it stops at the first segment that matches nothing.
func (c *conn) descend(dir string, segs []string) string {
	for _, seg := range segs {
		ranked := projects.Rank(seg, projects.Children(dir))
		if len(ranked) == 0 {
			break
		}
		dir = ranked[0].Path
	}
	return dir
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
		c.send(msgSay("nothing in " + filepath.Base(dir) + ", bud."))
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

// beginAttachQuestion prepares a session record for the chosen dir (in memory)
// and moves to the attach question. The record is only persisted once the user
// answers, so "cancel" leaves no junk behind.
func (c *conn) beginAttachQuestion(dir, prompt string) {
	base := sanitizeName(filepath.Base(dir))
	sess, err := c.newSession(base, dir)
	if err != nil {
		c.send(msgError("internal", err.Error()))
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
	case affirmative(text):
		sess := c.dlg.sess
		if perr := c.srv.store.Put(sess); perr != nil {
			c.send(msgError("internal", perr.Error()))
			c.dlg = nil
			return
		}
		c.dlg = nil
		if c.attached != nil {
			c.srv.unbindJob(c, c.attached.Name)
		}
		c.attached = sess
		c.srv.bindJob(c, sess.Name, true) // register for live turn fan-out (fresh session: no catch-up)
		c.send(msgAttached(sess.Name))
		c.send(msgSay("attached to " + sess.Name + "."))
	case negative(text):
		sess := c.dlg.sess
		if perr := c.srv.store.Put(sess); perr != nil {
			c.send(msgError("internal", perr.Error()))
			c.dlg = nil
			return
		}
		c.dlg = nil
		c.send(msgSay(sess.Name + " is ready when you are, bud."))
	default:
		c.send(msgSay("attach? yes or no, bud."))
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
func (s *Server) registerDiscovered(sessionID, dir string) (*session.Session, error) {
	rec := &session.Session{
		Name:      s.uniqueName(sanitizeName(filepath.Base(dir))),
		Dir:       dir,
		SessionID: sessionID,
		Started:   true,
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

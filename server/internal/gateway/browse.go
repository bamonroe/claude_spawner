package gateway

import (
	"os"
	"path/filepath"

	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
)

// doBrowse lists a directory for the app's visual "new session" picker. An empty
// path returns the roots; otherwise it returns the subfolders of path (jailed to
// a root), with a parent path for navigating up ("" = parent is the roots view).
func (c *conn) doBrowse(path string) {
	if path == "" {
		entries := make([]listingEntry, 0, len(c.srv.cfg.SpawnRoots))
		for _, r := range c.srv.cfg.SpawnRoots {
			entries = append(entries, listingEntry{Name: filepath.Base(r), Path: r, Repo: projects.IsRepo(r)})
		}
		c.send(msgListing("", "", entries))
		return
	}

	abs, err := c.srv.cfg.ValidateSpawnDir(path)
	if err != nil {
		c.fail("bad_path", err.Error())
		return
	}
	kids := projects.Children(abs)
	entries := make([]listingEntry, 0, len(kids))
	for _, d := range kids {
		entries = append(entries, listingEntry{Name: d.Name, Path: d.Path, Repo: projects.IsRepo(d.Path)})
	}
	parent := "" // at a root, "up" goes back to the roots view
	if !c.isRoot(abs) {
		parent = filepath.Dir(abs)
	}
	c.send(msgListing(abs, parent, entries))
}

// doSpawnAt creates a session in the chosen directory and attaches to it — the
// visual equivalent of finishing the spawn dialog.
func (c *conn) doSpawnAt(path string, target session.Target, create bool) {
	abs, err := c.srv.cfg.ValidateSpawnDir(path)
	if err != nil {
		c.fail("bad_path", err.Error())
		return
	}
	// create: make a brand-new project folder (jailed to a root) before spawning,
	// so the picker can start a session in a directory that doesn't exist yet.
	if create {
		if _, e := os.Stat(abs); e == nil {
			c.fail("bad_path", "that folder already exists")
			return
		}
		if e := os.MkdirAll(abs, 0o755); e != nil {
			c.fail("spawn_failed", e.Error())
			return
		}
	}
	if info, e := os.Stat(abs); e != nil || !info.IsDir() {
		c.fail("bad_path", "not a directory")
		return
	}
	if target == session.TargetSandbox && !c.srv.driver.SandboxEnabled() {
		c.fail("bad_path", "sandbox target requested but the sandbox target is not enabled")
		return
	}
	// Don't pile up a duplicate session for a directory that already has one — open
	// the existing session instead of minting a "-2". (This is the main source of
	// the confusing same-folder duplicates; a genuinely parallel session isn't
	// offered here — delete or rename the existing one first if you want a fresh id.)
	if existing := c.srv.store.GetByDir(abs); existing != nil {
		c.doAttach(existing.Name, false)
		return
	}
	sess, err := c.newSession(sanitizeName(filepath.Base(abs)), abs, target)
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	if perr := c.srv.store.Put(sess); perr != nil {
		c.fail("internal", perr.Error())
		return
	}
	c.ensureSandbox(sess) // start the persistent container for sandbox sessions
	if c.attached != nil {
		c.srv.unbindJob(c, c.attached.SessionID)
	}
	c.attached = sess
	c.srv.bindJob(c, sess, true)                        // register for live turn fan-out (fresh session: no catch-up)
	c.send(msgAttached(sess.Name, sess.SessionID, nil)) // freshly spawned: no transcript, no context size yet
	c.sendSessionList()
}

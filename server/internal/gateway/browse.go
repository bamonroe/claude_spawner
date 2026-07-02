package gateway

import (
	"os"
	"path/filepath"

	"github.com/bam/claude_spawner/server/internal/projects"
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
		c.send(msgError("bad_path", err.Error()))
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
func (c *conn) doSpawnAt(path string) {
	abs, err := c.srv.cfg.ValidateSpawnDir(path)
	if err != nil {
		c.send(msgError("bad_path", err.Error()))
		return
	}
	if info, e := os.Stat(abs); e != nil || !info.IsDir() {
		c.send(msgError("bad_path", "not a directory"))
		return
	}
	sess, err := c.newSession("claude-"+sanitizeName(filepath.Base(abs)), abs)
	if err != nil {
		c.send(msgError("internal", err.Error()))
		return
	}
	if perr := c.srv.store.Put(sess); perr != nil {
		c.send(msgError("internal", perr.Error()))
		return
	}
	c.attached = sess
	c.send(msgAttached(sess.Name))
	c.sendSessionList()
}

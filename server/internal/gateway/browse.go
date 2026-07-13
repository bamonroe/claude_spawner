package gateway

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
)

// doBrowse lists a directory for the app's visual "new session" picker, on the
// chosen host. Browsing is host-scoped: the listing runs on that host over SSH
// (loopback for LocalHost) so it reflects the machine the session will run on — not
// the server's own filesystem, which in a container is just a few mounts. An empty
// path starts at the host's filesystem root ("/"); there is no configured-roots
// jail here (the visual picker can walk the whole host). The response carries the
// parent for "up" navigation ("" at "/" — the top, nothing above it).
func (c *conn) doBrowse(path, host string, files bool) {
	if host == "" {
		host = session.LocalHost
	}
	dir := "/"
	if path != "" {
		dir = filepath.Clean(path)
	}
	entries, err := c.listDir(host, dir, files)
	if err != nil {
		c.fail("bad_path", err.Error())
		return
	}
	parent := ""
	if dir != "/" {
		parent = filepath.Dir(dir)
	}
	c.send(msgListing(dir, parent, entries))
}

// listDir returns dir's immediate entries on host. With files=false it lists only
// subdirectories (the new-session picker); with files=true it also includes regular
// files (the file-transfer picker). With SSH-native execution it probes the host
// over SSH; otherwise (SSH disabled, tests) it reads the local filesystem, which is
// then the same machine.
func (c *conn) listDir(host, dir string, files bool) ([]listingEntry, error) {
	if c.srv.ssh != nil {
		var des []session.DirEntry
		var err error
		if files {
			des, err = c.srv.ssh.ListAll(c.ctx, host, dir)
		} else {
			des, err = c.srv.ssh.ListDir(c.ctx, host, dir)
		}
		if err != nil {
			return nil, err
		}
		entries := make([]listingEntry, 0, len(des))
		for _, d := range des {
			entries = append(entries, listingEntry{Name: d.Name, Path: d.Path, Repo: d.Repo, Dir: d.Dir})
		}
		return entries, nil
	}
	kids := projects.Children(dir)
	entries := make([]listingEntry, 0, len(kids))
	for _, d := range kids {
		entries = append(entries, listingEntry{Name: d.Name, Path: d.Path, Repo: projects.IsRepo(d.Path), Dir: true})
	}
	if files {
		if des, err := os.ReadDir(dir); err == nil {
			for _, de := range des {
				if de.IsDir() || strings.HasPrefix(de.Name(), ".") {
					continue
				}
				if de.Type().IsRegular() {
					entries = append(entries, listingEntry{Name: de.Name(), Path: filepath.Join(dir, de.Name()), Dir: false})
				}
			}
		}
	}
	return entries, nil
}

// doSpawnAt creates a session in the chosen directory on the chosen host and
// attaches to it — the visual equivalent of finishing the spawn dialog. The
// directory (and its existence / creation) is resolved on the target host, so a
// remote spawn checks and makes the folder on that remote box, not locally.
func (c *conn) doSpawnAt(path string, target session.Target, create bool, host, agentID, model string) {
	dir := filepath.Clean(path)
	if !filepath.IsAbs(dir) {
		c.fail("bad_path", "spawn path must be absolute")
		return
	}
	// The execution location: a local sandbox (host stays empty) or a specific SSH
	// host — an unspecified host means loopback. This drives where the folder is
	// checked/created and which host the session pins to.
	wantHost := ""
	if target != session.TargetSandbox {
		wantHost = host
		if wantHost == "" {
			wantHost = session.LocalHost
		}
	}
	if target == session.TargetSandbox && !c.srv.driver.SandboxEnabled() {
		c.fail("bad_path", "sandbox target requested but the sandbox target is not enabled")
		return
	}

	exists, err := c.dirExists(wantHost, dir)
	if err != nil {
		c.fail("bad_path", err.Error())
		return
	}
	// create: make a brand-new project folder before spawning, so the picker can
	// start a session in a directory that doesn't exist yet.
	if create {
		if exists {
			c.fail("bad_path", "that folder already exists")
			return
		}
		if e := c.makeDir(wantHost, dir); e != nil {
			c.fail("spawn_failed", e.Error())
			return
		}
	} else if !exists {
		c.fail("bad_path", "not a directory")
		return
	}
	// Don't pile up a duplicate session for a directory that already runs in the same
	// place — open the existing session instead of minting a "-2". Match on directory
	// AND host: a folder may legitimately have one session per host, so a remote spawn
	// must not reuse the localhost session at the same path. Delete or rename the
	// existing one for a fresh id.
	if existing := c.srv.store.GetByDirHost(dir, wantHost); existing != nil {
		c.doAttach(existing.Name, false)
		return
	}
	sess, err := c.newSession(sanitizeName(filepath.Base(dir)), dir, target, agentID)
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	// An explicit model choice from the picker overrides the backend default that
	// newSession stamped — but only if it's a real model for this session's agent
	// (else keep the default rather than passing an unknown flag to the backend).
	if model != "" {
		if m, ok := c.srv.driver.AgentFor(sess).Model(model); ok {
			sess.Model = m.Alias
		}
	}
	// Pin the session to the resolved host. Sandbox sessions run in a local container,
	// so they keep no host; a host-target session records its (possibly loopback) host.
	if target != session.TargetSandbox {
		sess.Host = wantHost
	}
	if perr := c.srv.store.Put(sess); perr != nil {
		c.fail("internal", perr.Error())
		return
	}
	c.ensureSandbox(sess) // start the persistent container for sandbox sessions
	if c.attached != nil {
		c.srv.unbindJob(c, c.attached.SessionID)
	}
	c.setAttached(sess)
	c.srv.bindJob(c, sess, true)   // register for live turn fan-out (fresh session: no catch-up)
	c.send(msgAttached(sess, nil)) // freshly spawned: no transcript, no context size yet
	c.sendSessionList()
}

// dirExists reports whether dir is a directory in the given execution location.
// With SSH-native wired, EVERY location is checked over SSH — a host target on its
// host, and a sandbox target (host == "") on the loopback host, since the sandbox's
// container runs there and its files live on that host. Falls back to a local stat
// only when SSH isn't wired.
func (c *conn) dirExists(host, dir string) (bool, error) {
	if c.srv.ssh != nil {
		if host == "" {
			host = session.LocalHost
		}
		return c.srv.ssh.DirExists(c.ctx, host, dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	return true, nil
}

// makeDir creates dir (and parents) in the given execution location — over SSH on
// the target's host (a sandbox's empty host resolves to loopback, where its
// container's files live), or locally only when SSH isn't wired.
func (c *conn) makeDir(host, dir string) error {
	if c.srv.ssh != nil {
		if host == "" {
			host = session.LocalHost
		}
		return c.srv.ssh.MakeDir(c.ctx, host, dir)
	}
	return c.srv.driver.MakeSpawnDir(c.ctx, dir)
}

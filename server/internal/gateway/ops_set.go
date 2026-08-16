package gateway

import (
	"fmt"
	"strings"

	"github.com/bam/claude_spawner/server/internal/fuzzy"
	"github.com/bam/claude_spawner/server/internal/session"
)

// The "set" family gives voice control over the two shared settings that decide
// WHERE work happens: the target host (used both as the default spawn host and as
// the host a shell command runs on) and the default directory (where a spawn with
// no spoken path lands). Both persist through the shared-settings catalogue, so a
// value spoken once survives server restarts and syncs to every client.
//
// Each value is validated against the real world before it is stored — a target
// must name a configured host, a directory must resolve to a real directory on
// that host — so a mis-transcribed word can never leave the server pointing
// somewhere that doesn't exist. Every reply is one short spoken sentence.

// targetHost returns the configured target host, falling back to the local host
// when nothing has been set. This is the single reader for "which machine" —
// spawning and shell execution both go through it.
func (s *Server) targetHost() string {
	if h := strings.TrimSpace(s.settings.Value("target")); h != "" {
		return h
	}
	return session.LocalHost
}

// targetDir returns the configured default directory ("" when unset, meaning the
// caller should ask for a path instead of assuming one).
func (s *Server) targetDir() string { return strings.TrimSpace(s.settings.Value("directory")) }

// doSet stores a spoken setting after resolving its value against the target
// host's real world.
func (c *conn) doSet(key, value string) {
	if strings.TrimSpace(value) == "" {
		c.send(msgSay(fmt.Sprintf("set the %s to what?", key)))
		return
	}
	switch key {
	case "target":
		c.setTarget(value)
	case "directory":
		c.setDirectory(value)
	default:
		c.send(msgSay("i can set the target or the directory."))
	}
}

// setTarget resolves a spoken host name against the host catalogue (fuzzily, so a
// mis-heard name still lands) and persists it.
func (c *conn) setTarget(spoken string) {
	name, ok := c.resolveHostName(spoken)
	if !ok {
		c.send(msgSay(fmt.Sprintf("i don't have a host called %s. %s", spoken, knownHostsReply(c.hostNames()))))
		return
	}
	c.storeSetting("target", name)
	c.send(msgSay("target is now " + name + "."))
}

// setDirectory resolves a spoken path against the target host's filesystem (the
// same fuzzy walk the spawn dialog uses) and persists the resolved absolute path.
func (c *conn) setDirectory(spoken string) {
	segs, _ := parseSpokenPath(spoken)
	if len(segs) == 0 {
		c.send(msgSay("i didn't catch a path."))
		return
	}
	host := c.srv.targetHost()
	dir, _, res := c.resolveSpokenPath(host, segs)
	if res != pathOK {
		c.send(msgSay(fmt.Sprintf("i couldn't find %s on %s.", strings.Join(segs, " "), host)))
		return
	}
	c.storeSetting("directory", dir)
	c.send(msgSay("directory is now " + dir + "."))
}

// doGet speaks a setting's current value, naming the fallback when it's unset.
func (c *conn) doGet(key string) {
	switch key {
	case "target":
		c.send(msgSay("the target is " + c.srv.targetHost() + "."))
	case "directory":
		if dir := c.srv.targetDir(); dir != "" {
			c.send(msgSay("the directory is " + dir + "."))
		} else {
			c.send(msgSay("no directory is set — i'll ask for a path when you spawn."))
		}
	default:
		c.send(msgSay("i can tell you the target or the directory."))
	}
}

// storeSetting persists a server-originated setting change and re-syncs clients.
func (c *conn) storeSetting(key, value string) {
	if c.srv.persistSetting(key, value, nowMillis()) {
		c.srv.broadcastSettings()
	}
}

// hostNames lists every host a target may name: the catalogue plus the local host.
func (c *conn) hostNames() []string {
	names := []string{session.LocalHost}
	for _, h := range c.srv.hosts.List() {
		if h.Name != session.LocalHost {
			names = append(names, h.Name)
		}
	}
	return names
}

// resolveHostName maps a spoken host name onto a configured one, exactly first and
// then fuzzily, so "bahm" still finds "bam".
func (c *conn) resolveHostName(spoken string) (string, bool) {
	want := strings.Join(strings.Fields(strings.ToLower(spoken)), "")
	names := c.hostNames()
	for _, n := range names {
		if strings.ToLower(n) == want {
			return n, true
		}
	}
	for _, n := range names {
		if fuzzy.Equal(n, want) {
			return n, true
		}
	}
	return "", false
}

// knownHostsReply names the hosts that ARE configured, so a failed set tells the
// user what to say instead.
func knownHostsReply(names []string) string {
	if len(names) == 0 {
		return "no hosts are configured."
	}
	return "i know " + strings.Join(names, ", ") + "."
}

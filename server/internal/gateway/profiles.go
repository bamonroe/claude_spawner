package gateway

import (
	"errors"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The execution-profile catalogue (Settings → Profiles) is app-managed: the app is
// the source of truth, the server persists it (session.ProfileRegistry) so it
// survives restarts and is shared across clients. These handlers service the
// put/delete/set-default wire messages and broadcast the updated catalogue to every
// client on a change. The list itself is pushed on connect (msgProfiles), so there
// is no separate list-request message the way hosts/identities have.

// broadcastProfiles re-sends the full catalogue to every connected client.
func (c *conn) broadcastProfiles() {
	c.srv.broadcast(msgProfiles(c.srv.driver.ProfileRegistry()))
}

// doProfilePut adds or updates a profile (upsert by name) and broadcasts.
func (c *conn) doProfilePut(p *session.ExecProfile) {
	if p == nil || p.Name == "" {
		c.fail("bad_profile", "profile needs a name")
		return
	}
	if err := c.srv.driver.ProfileRegistry().Put(*p); err != nil {
		if errors.Is(err, session.ErrStale) {
			// A newer edit already won: re-send our catalogue so the stale client adopts it.
			c.send(msgProfiles(c.srv.driver.ProfileRegistry()))
			return
		}
		c.fail("bad_profile", err.Error())
		return
	}
	c.broadcastProfiles()
}

// doProfileDelete removes a profile by name (tombstoning it at updatedAt) and
// broadcasts. A delete older than the stored profile re-syncs the stale client.
func (c *conn) doProfileDelete(name string, updatedAt int64) {
	if name == "" {
		c.fail("bad_profile", "need a profile name to delete")
		return
	}
	if err := c.srv.driver.ProfileRegistry().Delete(name, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			// A newer edit already won: re-send our catalogue so the stale client adopts it.
			c.send(msgProfiles(c.srv.driver.ProfileRegistry()))
			return
		}
		c.fail("internal", err.Error())
		return
	}
	c.broadcastProfiles()
}

// doProfileSetDefault marks a profile as the default and broadcasts.
func (c *conn) doProfileSetDefault(name string) {
	if name == "" {
		c.fail("bad_profile", "need a profile name to set default")
		return
	}
	if err := c.srv.driver.ProfileRegistry().SetDefault(name); err != nil {
		c.fail("bad_profile", err.Error())
		return
	}
	c.broadcastProfiles()
}

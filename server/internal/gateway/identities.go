package gateway

import (
	"errors"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The SSH identity registry (Settings → Identities) is app-managed like the host
// registry: the app creates and names keypairs, the server generates and KEEPS the
// private key (it's what authenticates turns) and only ever hands back the public
// key for the user to copy onto a target host. These handlers service the
// list/create/delete wire messages and broadcast the updated list on a change.

// sendIdentityList returns the current identities (names + public keys) to the
// requesting client. Private keys are never included.
func (c *conn) sendIdentityList() {
	c.send(msgIdentityList(c.srv.ids.List()))
}

// doIdentityCreate registers a new identity (optionally generating a keypair) for the
// required user, with an optional password, and broadcasts the new list. A bad
// name/user or duplicate is a bad_identity error.
func (c *conn) doIdentityCreate(name, user, password string, genKey bool, updatedAt int64) {
	if name == "" || user == "" {
		c.fail("bad_identity", "identity needs a name and a username")
		return
	}
	if _, err := c.srv.ids.Create(name, user, password, genKey, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgIdentityList(c.srv.ids.List()))
			return
		}
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

// doIdentityImport registers an existing server-side private key as a managed
// identity (e.g. the config default key that's already authenticating turns) and
// broadcasts the new list. Bad name/path or an unreadable/encrypted key is a
// bad_identity error.
func (c *conn) doIdentityImport(name, user, password, keyPath string, updatedAt int64) {
	if name == "" || user == "" || keyPath == "" {
		c.fail("bad_identity", "import needs a name, a username, and a private-key path")
		return
	}
	if _, err := c.srv.ids.Import(name, user, password, keyPath, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgIdentityList(c.srv.ids.List()))
			return
		}
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

// doIdentityUpdate changes an existing identity's user and (when setPassword) its
// password, keeping the keypair, then broadcasts the new list.
func (c *conn) doIdentityUpdate(name, user string, setPassword bool, password string, updatedAt int64) {
	if name == "" || user == "" {
		c.fail("bad_identity", "update needs a name and a username")
		return
	}
	if _, err := c.srv.ids.Update(name, user, setPassword, password, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgIdentityList(c.srv.ids.List()))
			return
		}
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

// doIdentityDelete removes an identity and its private key, then broadcasts the new
// list. Hosts still referencing it fall back to their KeyFile / the ssh-agent.
func (c *conn) doIdentityDelete(name string, updatedAt int64) {
	if name == "" {
		c.fail("bad_identity", "need an identity name to delete")
		return
	}
	if err := c.srv.ids.Delete(name, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgIdentityList(c.srv.ids.List()))
			return
		}
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

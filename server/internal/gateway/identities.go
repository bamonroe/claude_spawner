package gateway

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

// doIdentityCreate generates a fresh keypair for the given name and broadcasts the
// new list. A duplicate name (or empty) is a bad_identity error — regenerating would
// invalidate any host already trusting the old public key.
func (c *conn) doIdentityCreate(name string) {
	if name == "" {
		c.fail("bad_identity", "identity needs a name")
		return
	}
	if _, err := c.srv.ids.Create(name); err != nil {
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

// doIdentityImport registers an existing server-side private key as a managed
// identity (e.g. the config default key that's already authenticating turns) and
// broadcasts the new list. Bad name/path or an unreadable/encrypted key is a
// bad_identity error.
func (c *conn) doIdentityImport(name, keyPath string) {
	if name == "" || keyPath == "" {
		c.fail("bad_identity", "import needs a name and a private-key path")
		return
	}
	if _, err := c.srv.ids.Import(name, keyPath); err != nil {
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

// doIdentityDelete removes an identity and its private key, then broadcasts the new
// list. Hosts still referencing it fall back to their KeyFile / the ssh-agent.
func (c *conn) doIdentityDelete(name string) {
	if name == "" {
		c.fail("bad_identity", "need an identity name to delete")
		return
	}
	if err := c.srv.ids.Delete(name); err != nil {
		c.fail("bad_identity", err.Error())
		return
	}
	c.srv.broadcast(msgIdentityList(c.srv.ids.List()))
}

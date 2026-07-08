package gateway

import "github.com/bam/claude_spawner/server/internal/session"

// The SSH host registry (Settings → Hosts) is app-managed: the app is the source of
// truth, the server persists it (session.HostStore) so it survives restarts and is
// shared across clients. These handlers service the list/put/delete wire messages
// and broadcast the updated list to every client on a change.

// sendHostList returns the current registry to the requesting client.
func (c *conn) sendHostList() {
	c.send(msgHostList(c.srv.hosts.List()))
}

// doHostPut adds or updates a host (upsert by name) and broadcasts the new list.
func (c *conn) doHostPut(h *session.Host) {
	if h == nil || h.Name == "" {
		c.fail("bad_host", "host needs a name")
		return
	}
	if err := c.srv.hosts.Put(h); err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.srv.broadcast(msgHostList(c.srv.hosts.List()))
}

// doHostDelete removes a host by name and broadcasts the new list.
func (c *conn) doHostDelete(name string) {
	if name == "" {
		c.fail("bad_host", "need a host name to delete")
		return
	}
	if err := c.srv.hosts.Delete(name); err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.srv.broadcast(msgHostList(c.srv.hosts.List()))
}

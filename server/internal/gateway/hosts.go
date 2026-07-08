package gateway

import (
	"log"

	"github.com/bam/claude_spawner/server/internal/session"
)

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
	// Trust-on-first-use: record the host's SSH key so the first turn/browse isn't
	// refused with "knownhosts: key is unknown". Best-effort — an unreachable host is
	// simply not recorded yet (re-saving once it's up retries); surface a soft notice
	// so the user knows to retry rather than hitting a silent failure later.
	if c.srv.ssh != nil {
		addr := h.Address
		if addr == "" {
			addr = h.Name
		}
		if err := c.srv.ssh.TrustHost(addr, h.Port); err != nil {
			log.Printf("host_put: trust key for %s (%s): %v", h.Name, addr, err)
			c.fail("bad_host", "saved, but couldn't reach "+addr+" to record its host key — save again once it's reachable")
		}
	}
}

// doHostDelete removes a host by name and broadcasts the new list.
func (c *conn) doHostDelete(name string) {
	if name == "" {
		c.fail("bad_host", "need a host name to delete")
		return
	}
	// Capture the address before deleting so we can also drop its known_hosts record.
	var addr string
	var port int
	if h := c.srv.hosts.Get(name); h != nil {
		addr = h.Address
		if addr == "" {
			addr = name
		}
		port = h.Port
	}
	if err := c.srv.hosts.Delete(name); err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.srv.broadcast(msgHostList(c.srv.hosts.List()))
	// Forget the host's key too, so deleting a host cleans up its trust record.
	if c.srv.ssh != nil && addr != "" {
		if err := c.srv.ssh.ForgetHost(addr, port); err != nil {
			log.Printf("host_delete: forget key for %s (%s): %v", name, addr, err)
		}
	}
}

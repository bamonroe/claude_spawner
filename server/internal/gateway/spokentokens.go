package gateway

import (
	"errors"

	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/spoken"
)

// The spoken-token catalogue (Settings → Spoken tokens) is app-managed: the app is
// the source of truth, the server persists it (session.SpokenTokenStore) so it
// survives restarts and is shared across clients. These handlers service the
// put/delete wire messages and broadcast the updated catalogue to every client on a
// change. The list itself is pushed on connect (msgSpokenTokens), like profiles, so
// there is no separate list-request message.

// broadcastSpokenTokens re-sends the full catalogue to every connected client.
func (c *conn) broadcastSpokenTokens() {
	c.srv.broadcast(msgSpokenTokens(c.srv.tokens.List()))
}

// doSpokenTokenPut adds or updates a token (upsert by name) and broadcasts. The
// token's action must be one of the known actions, else it's rejected.
func (c *conn) doSpokenTokenPut(t *spoken.Token) {
	if t == nil || t.Name == "" {
		c.fail("bad_token", "spoken token needs a name")
		return
	}
	if !spoken.IsAction(t.Action) {
		c.fail("bad_token", "unknown action: "+t.Action)
		return
	}
	if err := c.srv.tokens.Put(t); err != nil {
		if errors.Is(err, session.ErrStale) {
			// A newer edit already won: re-send our catalogue so the stale client adopts it.
			c.send(msgSpokenTokens(c.srv.tokens.List()))
			return
		}
		c.fail("bad_token", err.Error())
		return
	}
	c.broadcastSpokenTokens()
}

// doSpokenTokenDelete removes a token by name (tombstoning it at updatedAt) and
// broadcasts. A delete older than the stored record re-syncs the stale client.
func (c *conn) doSpokenTokenDelete(name string, updatedAt int64) {
	if name == "" {
		c.fail("bad_token", "need a token name to delete")
		return
	}
	if err := c.srv.tokens.Delete(name, updatedAt); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgSpokenTokens(c.srv.tokens.List()))
			return
		}
		c.fail("internal", err.Error())
		return
	}
	c.broadcastSpokenTokens()
}

package gateway

// The provider (AI-backend) settings overlay (Settings → Providers) is
// app-managed: the backends themselves are compile-time, but the app owns the
// per-backend overrides — the default model a spawn stamps and which models the
// voice commands enumerate — and the server persists them (agent.SettingsStore)
// so they survive restarts and are shared across clients. This handler services
// the provider_put wire message and re-broadcasts the enriched `agents` message
// (which now carries the effective default + per-model voice flags) to every
// client. The catalogue is pushed on connect via msgAgents, so there is no
// separate list-request message — the same as profiles.

// broadcastAgents re-sends the enriched backend catalogue to every client, so a
// provider override lands on all connected apps at once.
func (c *conn) broadcastAgents() {
	c.srv.broadcast(msgAgents(c.srv.driver.Registry(), c.srv.driver.ProviderSettings()))
}

// doProviderPut sets a backend's overrides (default model + the voice-enumerable
// model subset) and broadcasts. voiceModels is the exact enabled set the app
// sends; a nil slice (field absent) leaves the voice subset untouched at "all".
// The store validates every alias against the backend's real model catalogue.
func (c *conn) doProviderPut(agentID, defaultModel string, voiceModels []string) {
	if agentID == "" {
		c.fail("bad_provider", "need a backend id")
		return
	}
	if err := c.srv.driver.ProviderSettings().Put(agentID, defaultModel, voiceModels); err != nil {
		c.fail("bad_provider", err.Error())
		return
	}
	c.broadcastAgents()
}

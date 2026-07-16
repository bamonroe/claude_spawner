package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/bam/claude_spawner/server/internal/agent"
)

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

// modelRefreshTTL bounds how often a client connect triggers live model
// discovery, so a burst of reconnects doesn't spawn a probe each time.
const modelRefreshTTL = 30 * time.Second

// refreshModelsOnConnect re-discovers each backend's live model catalogue and, if
// a probe ran, re-broadcasts the backend list so models added to a backend (e.g.
// a new Ollama model wired into opencode) appear in already-connected apps with
// no server restart. Throttled to modelRefreshTTL and meant to run in its own
// goroutine off the connect path — the connect already sent the current catalogue
// synchronously, so this just delivers a fresher one moments later when warranted.
func (s *Server) refreshModelsOnConnect() {
	s.modelMu.Lock()
	if !s.modelRefreshed.IsZero() && time.Since(s.modelRefreshed) < modelRefreshTTL {
		s.modelMu.Unlock()
		return
	}
	s.modelRefreshed = time.Now()
	s.modelMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.driver.RefreshModels(ctx)
	s.broadcast(msgAgents(s.driver.Registry(), s.driver.ProviderSettings()))
}

// doProviderPut sets a backend's overrides (default model + the voice-enumerable
// model subset) and broadcasts. voiceModels is the exact enabled set the app
// sends; a nil slice (field absent) leaves the voice subset untouched at "all".
// The store validates every alias against the backend's real model catalogue.
func (c *conn) doProviderPut(agentID, defaultModel string, voiceModels []string, updatedAt int64) {
	if agentID == "" {
		c.fail("bad_provider", "need a backend id")
		return
	}
	if err := c.srv.driver.ProviderSettings().Put(agentID, defaultModel, voiceModels, updatedAt); err != nil {
		if errors.Is(err, agent.ErrStale) {
			// A newer edit already won: re-send the enriched catalogue so the client adopts it.
			c.send(msgAgents(c.srv.driver.Registry(), c.srv.driver.ProviderSettings()))
			return
		}
		c.fail("bad_provider", err.Error())
		return
	}
	c.broadcastAgents()
}

package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
)

// ErrStale is returned by SettingsStore.Put when the incoming provider override
// carries an `updated_at` older than the stored one — last-writer-wins rejects it
// and the gateway re-broadcasts the newer record. Mirrors session.ErrStale (the
// agent package can't import session, so it defines its own sentinel).
var ErrStale = errors.New("stale write rejected (older updated_at)")

// Settings is the app-managed overlay on an [Agent] (an AI backend). The backends
// are fixed in code and their model catalogue is either compiled in or discovered
// live from the backend ([Agent.Catalog]); either way the user can only override,
// per backend:
//
//   - DefaultModel — which model a fresh spawn stamps onto a new session;
//   - VoiceModels  — which models the voice "list models" / "use model N"
//     commands enumerate (so hard-to-say or redundant models can be hidden from
//     the spoken flow without removing them from the visual picker).
//
// One entry per agent id. A backend with no entry uses its compiled DefaultModel
// and enumerates all its models by voice.
type Settings struct {
	Agent        string   `json:"agent"`                   // agent id these settings apply to
	DefaultModel string   `json:"default_model,omitempty"` // model alias a fresh spawn stamps; "" = the agent's compiled DefaultModel
	VoiceModels  []string `json:"voice_models"`            // model aliases the voice commands enumerate, in agent order; nil = all
	// UpdatedAt is the client-stamped last-edit time in unix MILLISECONDS, driving
	// last-writer-wins arbitration. There is no provider delete, so no tombstone is
	// needed — only an older upsert is rejected. 0 = a pre-timestamp record.
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

// SettingsStore is a concurrency-safe, file-backed catalogue of per-backend
// [Settings]. The app is the source of truth (it edits the overrides), the server
// persists them to a JSON file and re-broadcasts the enriched `agents` message on
// change — mirroring ProfileRegistry / HostStore. It validates every override
// against the live [Registry] so a stored default/voice model always names a real
// model of that backend.
type SettingsStore struct {
	path string
	reg  *Registry
	mu   sync.RWMutex
	byID map[string]*Settings
}

// OpenSettingsStore loads the provider-settings overlay from path, validating
// each entry against reg. A missing file is a clean first run (no overrides). An
// existing file is read as either a JSON array or a {"providers":[...]} wrapper;
// entries for unknown agents or naming unknown models are dropped (the backend
// catalogue changed under a hand-edited file).
func OpenSettingsStore(path string, reg *Registry) (*SettingsStore, error) {
	s := &SettingsStore{path: path, reg: reg, byID: map[string]*Settings{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	list, err := parseSettings(data, path)
	if err != nil {
		return nil, err
	}
	for _, st := range list {
		if ag, ok := reg.Get(st.Agent); ok {
			s.byID[st.Agent] = sanitize(ag, st.DefaultModel, st.VoiceModels, st.UpdatedAt)
		}
	}
	return s, nil
}

func parseSettings(data []byte, path string) ([]Settings, error) {
	var list []Settings
	if err := json.Unmarshal(data, &list); err != nil {
		var wrapped struct {
			Providers []Settings `json:"providers"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse provider settings %s: %w", path, err)
		}
		list = wrapped.Providers
	}
	return list, nil
}

// sanitize builds a validated Settings for ag: default is kept only if it names a
// real model; voice is filtered to real aliases in the agent's own model order,
// deduped. A nil voice slice stays nil (meaning "all"); a non-nil one is honored
// as an exact subset (possibly empty = none).
func sanitize(ag *Agent, defaultModel string, voice []string, updatedAt int64) *Settings {
	st := &Settings{Agent: ag.ID, UpdatedAt: updatedAt}
	if defaultModel != "" {
		if _, ok := hasModel(ag, defaultModel); ok {
			st.DefaultModel = defaultModel
		}
	}
	if voice != nil {
		want := map[string]bool{}
		for _, a := range voice {
			want[a] = true
		}
		st.VoiceModels = []string{}
		for _, m := range ag.Catalog() { // agent order, deduped by construction
			if want[m.Alias] {
				st.VoiceModels = append(st.VoiceModels, m.Alias)
			}
		}
	}
	return st
}

// hasModel reports whether alias is one of the agent's canonical model aliases
// (not spoken forms — the settings overlay keys on the canonical alias only).
func hasModel(ag *Agent, alias string) (Model, bool) {
	for _, m := range ag.Catalog() {
		if m.Alias == alias {
			return m, true
		}
	}
	return Model{}, false
}

// get returns the stored settings for an agent id, or nil. Caller holds the lock.
func (s *SettingsStore) get(id string) *Settings {
	if s == nil {
		return nil
	}
	return s.byID[id]
}

// DefaultModel is the model alias a fresh spawn should stamp for ag: the user's
// override when set and valid, else the backend's compiled DefaultModel. Nil-safe.
func (s *SettingsStore) DefaultModel(ag *Agent) string {
	if s != nil {
		s.mu.RLock()
		st := s.byID[ag.ID]
		s.mu.RUnlock()
		if st != nil && st.DefaultModel != "" {
			return st.DefaultModel
		}
	}
	return ag.DefaultModel
}

// VoiceModels is the ordered subset of ag's models the voice "list models" /
// "use model N" commands should enumerate: the user's chosen subset when set,
// else all of ag's models. Nil-safe (unset store → all).
func (s *SettingsStore) VoiceModels(ag *Agent) []Model {
	var allow map[string]bool
	if s != nil {
		s.mu.RLock()
		st := s.byID[ag.ID]
		s.mu.RUnlock()
		if st != nil && st.VoiceModels != nil {
			allow = map[string]bool{}
			for _, a := range st.VoiceModels {
				allow[a] = true
			}
		}
	}
	models := ag.Catalog()
	if allow == nil {
		return models
	}
	out := make([]Model, 0, len(models))
	for _, m := range models {
		if allow[m.Alias] {
			out = append(out, m)
		}
	}
	return out
}

// VoiceEnabled reports whether the model alias is enumerated by voice for ag
// (used to badge the visual providers editor). Nil-safe (unset → all enabled).
func (s *SettingsStore) VoiceEnabled(ag *Agent, alias string) bool {
	for _, m := range s.VoiceModels(ag) {
		if m.Alias == alias {
			return true
		}
	}
	return false
}

// Put upserts the overrides for an agent and persists. The agent must exist;
// defaultModel (when non-empty) and every voice alias must name a real model of
// that agent, or it is an error. A nil voiceModels means "leave voice at all";
// pass a non-nil (possibly empty) slice to set an exact enumerated subset.
func (s *SettingsStore) Put(agentID, defaultModel string, voiceModels []string, updatedAt int64) error {
	ag, ok := s.reg.Get(agentID)
	if !ok {
		return fmt.Errorf("unknown backend %q", agentID)
	}
	if defaultModel != "" {
		if _, ok := hasModel(ag, defaultModel); !ok {
			return fmt.Errorf("backend %q has no model %q", agentID, defaultModel)
		}
	}
	for _, a := range voiceModels {
		if _, ok := hasModel(ag, a); !ok {
			return fmt.Errorf("backend %q has no model %q", agentID, a)
		}
	}
	s.mu.Lock()
	if cur := s.byID[agentID]; cur != nil && updatedAt < cur.UpdatedAt {
		s.mu.Unlock()
		return ErrStale
	}
	s.byID[agentID] = sanitize(ag, defaultModel, voiceModels, updatedAt)
	err := s.flush()
	s.mu.Unlock()
	return err
}

// UpdatedAt is the client-stamped last-edit time (unix ms) of ag's stored override,
// or 0 when there is none — included per-backend in the outbound `agents` message so
// the app can arbitrate last-writer-wins. Nil-safe.
func (s *SettingsStore) UpdatedAt(ag *Agent) int64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if st := s.byID[ag.ID]; st != nil {
		return st.UpdatedAt
	}
	return 0
}

// flush writes the overlay atomically. Caller holds s.mu.
func (s *SettingsStore) flush() error {
	if s.path == "" {
		return nil
	}
	out := make([]*Settings, 0, len(s.byID))
	for _, ag := range s.reg.List() { // stable, registration order
		if st := s.byID[ag.ID]; st != nil {
			out = append(out, st)
		}
	}
	data, err := json.MarshalIndent(map[string]any{"providers": out}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

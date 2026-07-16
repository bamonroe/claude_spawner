// Package agent is the registry of headless AI backends the server can drive.
//
// The server was originally hard-wired to the `claude` CLI: session.Driver.Turn
// built Claude's exact command line and parsed Claude's stream-json output. This
// package generalizes that. Each supported backend is an [Agent] that declares
// its identity, the models it offers (each by a short spoken alias like "opus"),
// a default model, how to build the per-turn command line ([Agent.Args]), and
// how to parse its output stream ([Agent.ParseTurn]). A session records which
// agent it uses and which model; the session Driver asks the agent to build the
// turn's args and parse the result rather than hardcoding Claude's behavior.
//
// Two backends ship today, each self-contained in its own file: Claude Code
// (claude.go) and Codex CLI (codex.go). Adding a backend is a new file in this
// package plus a [Default] registration — the full checklist lives in
// docs/architecture.md ("Adding an AI backend"). Where a turn runs (host /
// sandbox / SSH) stays the Executor's job — an Agent is orthogonal to it, so
// any backend runs on any target.
package agent

import (
	"io"
	"sync"
)

// TranscriptKind declares which on-disk transcript layout a backend writes, so
// the session driver can pick the matching reader for history replay, context
// usage, and deletion. A backend with a genuinely new layout adds a kind here
// and a reader in the session package (see codex_transcript.go for the model).
type TranscriptKind string

const (
	// TranscriptClaude is Claude Code's layout: ~/.claude/projects/<dir-slug>/<session_id>.jsonl.
	TranscriptClaude TranscriptKind = "claude-projects"
	// TranscriptCodex is Codex CLI's layout: ~/.codex/sessions/YYYY/MM/DD/rollout-*-<thread_id>.jsonl.
	TranscriptCodex TranscriptKind = "codex-rollouts"
	// TranscriptOpencode is opencode's layout: sessions live in a SQLite database
	// (~/.local/share/opencode/opencode.db), not flat files, so its reader shells
	// out to opencode's own `export`/`session delete` commands rather than reading
	// a path (see opencode_transcript.go).
	TranscriptOpencode TranscriptKind = "opencode-db"
	// TranscriptAntigravity is the Antigravity (agy) layout: conversations live in
	// per-id SQLite databases plus a brain/<internal-id>/.system_generated/logs/
	// transcript.jsonl, keyed by an internal id we don't hold (agy maps our
	// caller-supplied --conversation uuid to it). No reader is wired yet, so this
	// kind routes to the null transcript reader — agy's spoken reply streams live
	// off stdout, but past-turn history replay/context/deletion are not backed by
	// its on-disk store. See antigravity.go and null_transcript.go.
	TranscriptAntigravity TranscriptKind = "antigravity"
)

// Model is one selectable model within an [Agent], chosen by a short alias the
// user can say or type ("opus", "sonnet", "fable"). Flag is the concrete value
// handed to the backend's model flag — an alias the CLI accepts, or a full model
// id when the alias is ambiguous.
type Model struct {
	Alias  string   // canonical alias, unique within the Agent
	Flag   string   // value passed to the backend's model flag (e.g. "opus")
	Spoken []string // extra spoken/typed forms that also resolve here (e.g. "fable five")
	// Args, when set, is the exact CLI fragment this model contributes, used
	// instead of the backend's default "<model-flag> <Flag>" convention. It lets a
	// backend express choices its model flag can't — e.g. Codex encoding a
	// reasoning-effort preset as "-m gpt-5.5 -c model_reasoning_effort=high". Empty
	// falls back to Flag.
	Args []string
}

// TurnSpec is everything an [Agent] needs to build one turn's command line. It
// is location-independent: the Executor owns where the process runs, the Agent
// owns what it's invoked with.
type TurnSpec struct {
	Prompt    string // the dictation/prompt for this turn
	SessionID string // the backend's session/conversation id
	Resume    bool   // false: first turn (create the session); true: reattach
	Model     string // model alias; "" resolves to the Agent's DefaultModel
	Bypass    bool   // add the backend's skip-permissions flag
	// Dir is the session's working directory. Most backends inherit it as the
	// process cwd (the Executor sets it) and ignore this field; Antigravity (agy)
	// ignores cwd and instead needs the workspace passed explicitly (--add-dir),
	// so its build reads Dir. Empty means "no explicit workspace".
	Dir string
	// SettingsJSON is a backend-settings JSON string injected at launch (Claude's
	// --settings). The server uses it to install the PreToolUse hook that enforces
	// detached background jobs. Empty for backends/turns that don't need it; only
	// the Claude backend consumes it.
	SettingsJSON string
}

// Agent is a headless AI CLI backend the server can drive. Each Agent is fully
// self-contained: it declares its identity and models, builds its own command
// line, and parses its own output stream — no caller branches on which backend
// it is.
type Agent struct {
	// ID is the stable identifier persisted on a session (e.g. "claude"). It
	// keys the registry and never changes once sessions reference it.
	ID string
	// Name is the human-facing display name (e.g. "Claude Code").
	Name string
	// Bin is the backend's default command (path or PATH name), e.g. "codex". The
	// session Driver passes it to the Executor as the process to launch. Empty
	// means "defer to the executor's own configured binary" — Claude leaves this
	// empty so every target keeps using its SPAWNER_*_CLAUDE_BIN as before.
	Bin string
	// Transcript is the backend's on-disk transcript layout, selecting the
	// session-side reader for history replay / context usage / deletion.
	Transcript TranscriptKind
	// SelfAssignsID reports that the backend mints its own session id (announced
	// in its output stream, surfaced via TurnResult.SessionID) rather than
	// accepting a caller-supplied one. Claude takes an id (--session-id), so
	// false; Codex assigns a thread_id on the first turn, so true — the session
	// driver adopts it from the TurnResult and persists it, and only marks the
	// session "started" once it has.
	SelfAssignsID bool
	// DefaultModel is the alias stamped onto a new session when the spawner picks
	// for you. Must name one of Models. Spawn uses it so every session has an
	// explicit model; voice can override later.
	DefaultModel string
	// Models is the ordered catalogue of selectable models the backend ships with
	// (first is conventionally the strongest/default). It is the *fallback* list:
	// when the backend also supports live discovery (DiscoverArgs) and a probe
	// succeeds, the discovered catalogue replaces it — see Catalog. A backend with
	// no discovery, or whose probe fails, always presents this compiled list.
	Models []Model
	// DiscoverArgs, when non-empty, is the argv (after the backend's binary) of a
	// command whose stdout lists the models the backend can currently run, one per
	// line — the mechanism that lets a backend report its live catalogue (e.g.
	// opencode's `models ollama`) instead of relying only on the compiled Models.
	// The session layer runs it on the host and hands the stdout to ParseModels.
	// Empty means "no discovery — always use Models".
	DiscoverArgs []string
	// ParseModels turns the stdout of the DiscoverArgs command into a model
	// catalogue. Required iff DiscoverArgs is set. Returning an empty slice (e.g.
	// unparseable output) is treated as "discovery yielded nothing" and the
	// compiled Models are kept.
	ParseModels func(stdout []byte) []Model
	// mu guards the discovered catalogue below, which RefreshModels writes at
	// runtime while turns read it via Catalog.
	mu sync.RWMutex
	// dynamic is the last successfully discovered catalogue; nil until a probe
	// succeeds. Once set it shadows Models everywhere the catalogue is read.
	dynamic []Model
	// ParseTurn consumes one turn's stdout stream until EOF and returns the clean
	// reply, token usage, and (for self-assigning backends) the session id the
	// stream announced. Live events fan out via the callbacks. Required — the
	// session driver refuses to run an agent without one.
	ParseTurn func(r io.Reader, cb TurnCallbacks) (TurnResult, error)
	// build returns the per-turn command-line args (excluding the binary), given a
	// spec and this agent's resolved model. Per-backend, since flags differ.
	build func(a *Agent, s TurnSpec, m Model) []string
}

// Catalog is the agent's effective model list: the discovered catalogue when a
// probe has succeeded (see RefreshModels), else the compiled Models. Every read
// of an agent's models — resolution, the settings overlay, the `agents` wire
// message — goes through here, so live discovery transparently takes over. Safe
// for concurrent use with the runtime refresh.
func (a *Agent) Catalog() []Model {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.dynamic != nil {
		return a.dynamic
	}
	return a.Models
}

// CanDiscover reports whether the backend supports live model discovery (it
// declares both a probe command and a parser).
func (a *Agent) CanDiscover() bool {
	return len(a.DiscoverArgs) > 0 && a.ParseModels != nil
}

// SetDiscovered installs a discovered catalogue as the agent's effective model
// list, shadowing the compiled Models. An empty/nil list is ignored, so a probe
// that yields nothing leaves the fallback list in place.
func (a *Agent) SetDiscovered(models []Model) {
	if len(models) == 0 {
		return
	}
	a.mu.Lock()
	a.dynamic = models
	a.mu.Unlock()
}

// Model resolves an alias or spoken form to one of the agent's models. The empty
// string (or an unknown alias) resolves to DefaultModel. The bool reports whether
// the input matched a real model (false = fell back to the default).
func (a *Agent) Model(alias string) (Model, bool) {
	models := a.Catalog()
	if alias != "" {
		for _, m := range models {
			if m.Alias == alias {
				return m, true
			}
			for _, s := range m.Spoken {
				if s == alias {
					return m, true
				}
			}
		}
	}
	for _, m := range models {
		if m.Alias == a.DefaultModel {
			return m, false
		}
	}
	// No DefaultModel match (misconfigured agent, or the default isn't in the
	// discovered catalogue): fall back to the first model, or a zero Model if
	// there are none.
	if len(models) > 0 {
		return models[0], false
	}
	return Model{}, false
}

// Args builds the full command line (excluding the binary) for one turn. An
// empty spec Model means "omit the model flag" — the session stores no model, so
// the backend uses its own configured default (this preserves how sessions
// created before the model field ran). The DefaultModel is applied at spawn time
// (stamped into the session), not forced here on every turn; a non-empty spec
// Model is resolved against this agent's catalogue.
func (a *Agent) Args(s TurnSpec) []string {
	var m Model
	if s.Model != "" {
		m, _ = a.Model(s.Model)
	}
	return a.build(a, s, m)
}

// Registry is the set of known backends, in registration order. It is
// constructed once at startup ([Default]) and read-only thereafter.
type Registry struct {
	order []*Agent
	byID  map[string]*Agent
}

// Get returns the agent with the given id, or (nil, false).
func (r *Registry) Get(id string) (*Agent, bool) {
	a, ok := r.byID[id]
	return a, ok
}

// Resolve returns the agent for id, falling back to the [Registry.Default] agent
// when id is empty or unknown — so sessions predating the agent field (empty id)
// and unknown ids both run on the default backend rather than failing.
func (r *Registry) Resolve(id string) *Agent {
	if a, ok := r.byID[id]; ok {
		return a
	}
	return r.Default()
}

// Default is the agent a new session gets when none is chosen, and the fallback
// for an empty/unknown id. It is the first registered agent.
func (r *Registry) Default() *Agent {
	if len(r.order) == 0 {
		return nil
	}
	return r.order[0]
}

// List returns the registered agents in registration order.
func (r *Registry) List() []*Agent { return r.order }

// Default returns the registry of backends the server ships with. Claude is
// registered first, so it is the default backend for new sessions.
func Default() *Registry {
	r := &Registry{byID: map[string]*Agent{}}
	r.register(claude())
	r.register(codex())
	r.register(opencode())
	r.register(antigravity())
	return r
}

func (r *Registry) register(a *Agent) {
	r.order = append(r.order, a)
	r.byID[a.ID] = a
}

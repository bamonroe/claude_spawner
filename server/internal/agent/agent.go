// Package agent is the registry of headless AI backends the server can drive.
//
// The server was originally hard-wired to the `claude` CLI: session.Driver.Turn
// built Claude's exact command line and parsed Claude's stream-json output. This
// package generalizes that. Each supported backend is an [Agent] that declares
// its identity, the models it offers (each by a short spoken alias like "opus"),
// a default model, and how to build the per-turn command line ([Agent.Args]). A
// session records which agent it uses and which model; the session Driver asks
// the agent to build the turn's args rather than hardcoding Claude's flags.
//
// Only the "claude" backend ships today, but the seam is real and load-bearing:
// adding a second backend is a new [Agent] entry in [Default] plus, if its
// output isn't Claude's stream-json, a parser for its [Format] on the session
// side. Where a turn runs (host / sandbox / SSH) stays the Executor's job — an
// Agent is orthogonal to it, so any backend runs on any target.
package agent

// Format identifies how a backend streams its output, so the session driver can
// select the matching parser. Only Claude's stream-json exists today; a second
// backend with a different output shape adds a Format here and a parser that
// switches on it.
type Format string

// FormatClaudeStreamJSON is Claude Code's `--output-format stream-json` NDJSON:
// a `result` event carries the clean reply and token usage. See session.parseStream.
const FormatClaudeStreamJSON Format = "claude-stream-json"

// FormatCodexJSONL is Codex CLI's `codex exec --json` output: a `thread.started`
// event carries the session id (thread_id), `item.completed` agent_message items
// carry the reply text, and `turn.completed` carries token usage. See
// session.parseCodexStream.
const FormatCodexJSONL Format = "codex-jsonl"

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
}

// Agent is a headless AI CLI backend the server can drive.
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
	// Format is this backend's output shape, selecting the parser on the session side.
	Format Format
	// SelfAssignsID reports that the backend mints its own session id (read from
	// its output) rather than accepting a caller-supplied one. Claude takes an id
	// (--session-id), so false; Codex assigns a thread_id on the first turn, so
	// true — the session driver captures it from the stream and persists it, and
	// only marks the session "started" once it has.
	SelfAssignsID bool
	// DefaultModel is the alias stamped onto a new session when the spawner picks
	// for you. Must name one of Models. Spawn uses it so every session has an
	// explicit model; voice can override later.
	DefaultModel string
	// Models is the ordered catalogue of selectable models (first is conventionally
	// the strongest/default).
	Models []Model
	// build returns the per-turn command-line args (excluding the binary), given a
	// spec and this agent's resolved model. Per-backend, since flags differ.
	build func(a *Agent, s TurnSpec, m Model) []string
}

// Model resolves an alias or spoken form to one of the agent's models. The empty
// string (or an unknown alias) resolves to DefaultModel. The bool reports whether
// the input matched a real model (false = fell back to the default).
func (a *Agent) Model(alias string) (Model, bool) {
	if alias != "" {
		for _, m := range a.Models {
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
	for _, m := range a.Models {
		if m.Alias == a.DefaultModel {
			return m, false
		}
	}
	// No DefaultModel match (misconfigured agent): fall back to the first model,
	// or a zero Model if there are none.
	if len(a.Models) > 0 {
		return a.Models[0], false
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
	return r
}

func (r *Registry) register(a *Agent) {
	r.order = append(r.order, a)
	r.byID[a.ID] = a
}

// claude builds the Claude Code backend entry. Its Args reproduce the command
// line session.Driver.Turn used before the registry existed, plus the model flag.
func claude() *Agent {
	return &Agent{
		ID:           "claude",
		Name:         "Claude Code",
		Format:       FormatClaudeStreamJSON,
		DefaultModel: "opus",
		Models: []Model{
			// Behaviour-preserving default: opus matches how the CLI was already
			// being driven here. Aliases opus/sonnet are what `claude --model`
			// accepts directly; fable uses the full id to avoid alias ambiguity.
			{Alias: "opus", Flag: "opus"},
			{Alias: "sonnet", Flag: "sonnet"},
			{Alias: "fable", Flag: "claude-fable-5", Spoken: []string{"fable five", "fable5"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			args := []string{"-p", s.Prompt, "--output-format", "stream-json", "--verbose"}
			if s.Resume {
				args = append(args, "--resume", s.SessionID)
			} else {
				args = append(args, "--session-id", s.SessionID)
			}
			if s.Bypass {
				args = append(args, "--dangerously-skip-permissions")
			}
			if len(m.Args) > 0 {
				args = append(args, m.Args...)
			} else if m.Flag != "" {
				args = append(args, "--model", m.Flag)
			}
			return args
		},
	}
}

// codex builds the Codex CLI backend entry. Codex runs non-interactively via
// `codex exec`; unlike Claude it mints its OWN session id (the thread_id, read
// from the first output event) rather than accepting a caller-supplied one, so
// the first turn omits any id and the session driver captures thread_id from the
// stream. Resume replays via `codex exec resume <id>`. The working directory is
// set by the Executor (the process cwd), so no -C is needed. On this account the
// supported model is gpt-5.5; the alternates are reasoning-effort presets on it
// (plan-independent), which is why ordinal selection ("use model 2") matters —
// the labels are awkward to say.
func codex() *Agent {
	return &Agent{
		ID:            "codex",
		Name:          "Codex CLI",
		Bin:           "codex",
		Format:        FormatCodexJSONL,
		SelfAssignsID: true,
		DefaultModel:  "gpt-5.5",
		Models: []Model{
			{Alias: "gpt-5.5", Flag: "gpt-5.5", Spoken: []string{"five five", "gpt five five", "standard"}},
			{Alias: "gpt-5.5-high", Args: []string{"-m", "gpt-5.5", "-c", "model_reasoning_effort=high"}, Spoken: []string{"high", "high reasoning", "thorough"}},
			{Alias: "gpt-5.5-low", Args: []string{"-m", "gpt-5.5", "-c", "model_reasoning_effort=low"}, Spoken: []string{"low", "low reasoning", "fast"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			args := []string{"exec"}
			if s.Resume {
				args = append(args, "resume", s.SessionID)
			}
			// Options before the positional prompt; `--` terminates flags so a dictated
			// prompt starting with "-" (or the word "resume") can't be misparsed as one.
			args = append(args, "--json", "--skip-git-repo-check")
			if s.Bypass {
				args = append(args, "--dangerously-bypass-approvals-and-sandbox")
			}
			if len(m.Args) > 0 {
				args = append(args, m.Args...)
			} else if m.Flag != "" {
				args = append(args, "-m", m.Flag)
			}
			args = append(args, "--", s.Prompt)
			return args
		},
	}
}

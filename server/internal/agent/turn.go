package agent

import (
	"bufio"
	"io"
)

// This file holds the backend-neutral vocabulary of one turn: the callback and
// result types every [Agent.ParseTurn] implementation speaks, and the shared
// line scanner they (and the session package's transcript readers) use. The
// per-backend parsers themselves live with their Agent (claude.go, codex.go).

// ToolUse describes a tool the backend invoked during a turn. FilePath is set
// for file-editing tools so the caller can show which file changed.
type ToolUse struct {
	Name     string
	FilePath string
}

// Usage is the token accounting for one turn. CacheRead > 0 means the turn
// reused a warm prompt cache (cheap/fast); CacheWrite > 0 means it (re)built
// the cache — the signal behind the app's cache-warm indicator. The zero value
// means the turn reported no usage. The json tags are the on-wire names sent to
// the app (see the `output` message in docs/protocol.md).
type Usage struct {
	Input      int `json:"input"`       // fresh input tokens
	Output     int `json:"output"`      // output tokens (incl. reasoning, where reported)
	CacheWrite int `json:"cache_write"` // cache (re)built
	CacheRead  int `json:"cache_read"`  // warm-cache hit
}

// RateLimit is the subscription usage-window state a backend may report during
// a turn (Claude's stream-json `rate_limit_event`). It is how the app shows the
// plan's session limit. Status is a COARSE signal — "allowed" until you
// near/hit the cap. ResetsAt is unix seconds; Type names the binding window
// ("five_hour" | weekly). The zero value (empty Type) means the turn carried no
// rate-limit event.
type RateLimit struct {
	Status       string `json:"status"`        // "allowed" | warning/rejected as the cap nears
	ResetsAt     int64  `json:"resets_at"`     // unix seconds when this window resets
	Type         string `json:"limit_type"`    // "five_hour" (rolling session) | weekly
	UsingOverage bool   `json:"using_overage"` // currently drawing on pay-as-you-go overage
}

// TurnCallbacks fan a turn's live events out to the caller while ParseTurn
// consumes the stream. Any callback may be nil.
type TurnCallbacks struct {
	OnTool      func(ToolUse)   // each tool/step breadcrumb, in stream order
	OnText      func(string)    // each assistant prose message as it lands
	OnRateLimit func(RateLimit) // subscription window state, if the stream reports it
}

// TurnResult is what a completed turn parsed out of the backend's stream.
type TurnResult struct {
	Reply string // the clean final reply text
	Usage Usage  // the turn's token accounting (zero if unreported)
	// SessionID is the backend-minted session id announced in the stream — only
	// set by self-assigning backends (Codex's thread_id). Parsers return it even
	// on the error path (the id event precedes any failure), so a first turn that
	// fails mid-way is still resumable rather than re-created. Empty for backends
	// that accept a caller-supplied id (Claude).
	SessionID string
}

// NewLineScanner returns a bufio.Scanner for newline-delimited JSON. It starts
// with a modest 64 KB buffer but allows lines to grow to 16 MB, since a single
// tool-use event's input can far exceed bufio's default 64 KB line cap. Shared
// by every backend's stream parser and the session package's transcript readers.
func NewLineScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	return sc
}

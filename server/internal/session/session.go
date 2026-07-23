// Package session drives headless AI backend turns and tracks sessions as
// durable records (a session_id on disk tied to a directory), not live
// processes. This is the data path for the voice interface: each dictated turn
// shells out to the session's backend CLI (Claude Code's `claude -p
// --output-format stream-json` by default — see internal/agent for the backend
// registry) and the clean reply text is returned for text-to-speech. See
// docs/protocol.md and the "TUI capture" decision in CLAUDE.md.
package session

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// newLineScanner is the JSONL scanner shared by the transcript/discover
// readers; it lives in the agent package alongside the backend stream parsers
// that also use it.
var newLineScanner = agent.NewLineScanner

// Session is a durable record. There is no long-lived process: the conversation
// state lives on disk under SessionID and is reattached via `claude --resume`.
type Session struct {
	Name      string `json:"name"`       // human/voice handle, e.g. "claude-claude"
	Dir       string `json:"dir"`        // working directory for the session
	SessionID string `json:"session_id"` // claude session uuid (generated at spawn)
	Started   bool   `json:"started"`    // false until the first turn has run
	// AskPrimed records that the interactive-mode ask instruction has been sent to
	// Claude for the current context, so later turns don't re-append it (Claude
	// keeps it via --resume). Reset by "clear"/"compress", which rotate the context.
	AskPrimed bool `json:"ask_primed,omitempty"`
	// PriorIDs are session_ids retired by "clear"/"compress" (context rotation),
	// oldest first. Their transcripts stay on disk so the app can show the full
	// history, but Claude only ever resumes the current SessionID — so a rotation
	// changes context without losing (or re-reading) the record.
	PriorIDs []string `json:"prior_ids,omitempty"`
	// PendingSeed is a condensed summary of the prior context, produced by
	// "compress" when it rotated the session_id. It is prepended to the FIRST
	// dictation on the fresh SessionID (so Claude continues with the compacted
	// context) and then cleared. Empty except in the window between a compress and
	// the next dictation. "clear" wipes it (a clear means truly empty context).
	PendingSeed string `json:"pending_seed,omitempty"`
	// Target selects where this session's turns run: TargetHost (direct exec on the
	// host — real host files/toolchains) or TargetSandbox (an isolated container).
	// Chosen at spawn time and durable. Empty means host (records predate this
	// field). Turn resolves it to a registered Executor. See docs/architecture.md.
	Target Target `json:"target,omitempty"`
	// Container is the name of the persistent sandbox container bound to this
	// session's lifetime (sandbox target only): created at spawn, reused every turn,
	// removed on delete. Empty for host sessions.
	Container string `json:"container,omitempty"`
	// Host is the SSH target where this session's turns run under SSH-native
	// execution: empty means the local machine (loopback), a name like "work" means
	// that remote box. SSHExecutor reads it to pick the pooled connection. Reserved:
	// the spawn-dialog choice and Driver routing that select the SSH executor land in
	// a later commit of the SSH-native epic (see TODO.md); today nothing sets it.
	Host string `json:"host,omitempty"`
	// Agent is the id of the AI backend this session runs (agent.Registry). Empty
	// means the default backend — records predate this field, and it keeps old
	// sessions on Claude. Chosen at spawn time and durable. Turn resolves it to a
	// registered agent.Agent, which builds the turn's command line.
	Agent string `json:"agent,omitempty"`
	// Model is the backend model alias for this session (e.g. "opus", "sonnet",
	// "fable"). Empty means the backend's own configured default (no --model flag);
	// spawn stamps the agent's DefaultModel here, and a voice command can change it.
	Model string `json:"model,omitempty"`
	// Profile is the execution-environment profile name. Empty means the built-in
	// default profile, preserving records written before profiles existed.
	Profile string `json:"profile,omitempty"`
	// ResolvedProfile is set by Driver immediately before launching a turn or
	// sandbox lifecycle command. It is deliberately not persisted.
	ResolvedProfile *ExecProfile `json:"-"`
	// Jobs mirrors the detached background jobs Claude launched for this session via
	// the spawner-job wrapper (see internal/session/bgjob). The reconciler diffs the
	// on-target registry against this list at turn boundaries; a job that just
	// finished has its log tail injected into the next turn and is marked Notified.
	// Because the on-target registry is keyed by Dir (not session_id), Jobs ride the
	// struct through clear/compress session_id rotation and MUST NOT be wiped by a
	// context clear — a background job outlives a context reset.
	Jobs []BackgroundJob `json:"jobs,omitempty"`
	// PendingNotes are framed completion notes for finished background jobs, waiting
	// to be prepended to the next dictation so Claude learns a job it started earlier
	// has finished (with a bounded log tail). Cleared once injected. Like Jobs, this
	// survives a context clear.
	PendingNotes []string `json:"pending_notes,omitempty"`
	// JobsPrimed records that the background-job instruction (use spawner-job for
	// long-running commands) has been sent to Claude for the current context, so it
	// isn't re-appended every turn. Reset by clear/compress like AskPrimed
	// (re-priming after a context rotation is harmless).
	JobsPrimed bool `json:"jobs_primed,omitempty"`
	// AgyBrainIDs records, in turn order, the antigravity "brain" directory id each
	// turn wrote under ~/.gemini/antigravity-cli/brain/<id>/. Unlike Claude/Codex,
	// agy IGNORES the --conversation id we pass and files every turn under a fresh
	// internal id of its own, so we can't recover a session's history by our own
	// session_id. Instead we capture the brain id when we locate a turn's transcript
	// to rebuild its reply (reconstructAgyReply), building our own ordered map from
	// this session to agy's on-disk turns — the antigravity history reader replays
	// these. Empty for non-antigravity sessions. Like Jobs, it rides through the
	// clear/compress session_id rotation (its transcripts stay on disk for scrollback).
	AgyBrainIDs []string `json:"agy_brain_ids,omitempty"`
	// History holds the display transcripts of PREVIOUS backends this session ran,
	// oldest first — archived each time set_agent switches the backend. A switch
	// rotates to a fresh session_id and drops the old chain from the CONTEXT the new
	// backend reads (their on-disk formats are incompatible), but the old messages
	// stay on disk and belong in the chat log. Each segment records the backend +
	// host that wrote its ids so the display path reads it with the RIGHT reader
	// (a Codex segment via the Codex reader, etc.) and concatenates — see
	// Driver.ReadDisplayHistory. Distinct from PriorIDs, which is the SAME-backend
	// clear/compress rotation chain of the current backend. Empty for a session that
	// has never switched backends (behaves exactly as before).
	History []HistorySegment `json:"history,omitempty"`
}

// HistorySegment is one previous backend's slice of a session's display history:
// the ids under which that backend stored its transcripts (a session_id chain for
// Claude/Codex/opencode, brain-dir ids for antigravity), tagged with the backend
// and host so Driver.ReadDisplayHistory reads each with the matching reader.
type HistorySegment struct {
	Agent string   `json:"agent,omitempty"` // AI backend id that wrote these transcripts
	Host  string   `json:"host,omitempty"`  // SSH host they live on (empty = local)
	IDs   []string `json:"ids"`             // that backend's transcript ids, oldest first
}

// BackgroundJob is one detached job Claude launched via the spawner-job wrapper,
// as tracked by the server across turns. The authoritative live state is the
// on-target registry (keyed by Dir); this is the server's view of which jobs it has
// already told Claude about, so a finished job is announced exactly once.
type BackgroundJob struct {
	ID       string `json:"id"`                  // spawner-job registry id (epoch_pid_rand)
	Cmd      string `json:"cmd"`                 // the shell command it runs
	Started  int64  `json:"started"`             // epoch seconds it was launched
	Done     bool   `json:"done"`                // observed finished by the reconciler
	ExitCode int    `json:"exit_code,omitempty"` // best-effort (detached jobs report 0)
	Notified bool   `json:"notified"`            // its completion note was injected already
	// Session is the session_id that launched the job. The on-target registry is
	// dir-keyed, so several sessions in one directory see each other's jobs; the
	// reconciler uses this to adopt/announce only jobs THIS session owns (matched
	// via OwnsID against the session_id chain, since the id may have rotated). Empty
	// for a legacy job started before jobs were stamped — those stay dir-attributed.
	Session string `json:"session,omitempty"`
}

// OwnsID reports whether id is a transcript session_id this session has ever run
// under: its current SessionID, an id retired by clear/compress (PriorIDs), or an
// id archived by a backend switch (History). Used to attribute a dir-keyed
// background job to the session that launched it — the job stamps the session_id
// current at launch, which may since have rotated, so a single-id check isn't
// enough.
func (s *Session) OwnsID(id string) bool {
	if id == "" {
		return false
	}
	if id == s.SessionID {
		return true
	}
	for _, p := range s.PriorIDs {
		if p == id {
			return true
		}
	}
	for _, seg := range s.History {
		for _, hid := range seg.IDs {
			if hid == id {
				return true
			}
		}
	}
	return false
}

// TranscriptIDs returns every session_id whose transcript belongs to this
// session, oldest first: ids retired by "clear" followed by the current one.
// Used to assemble the full history for display without Claude re-reading it.
func (s *Session) TranscriptIDs() []string {
	ids := make([]string, 0, len(s.PriorIDs)+1)
	ids = append(ids, s.PriorIDs...)
	ids = append(ids, s.SessionID)
	return ids
}

// HasPriorID reports whether id is one of the session_ids this session retired via
// a "clear"/"compress" context rotation (see PriorIDs). It does NOT match the
// current SessionID — callers check that separately.
func (s *Session) HasPriorID(id string) bool {
	for _, prior := range s.PriorIDs {
		if prior == id {
			return true
		}
	}
	return false
}

// NewContainerName returns a unique sandbox container name ("spawner-sbx-<hex>"),
// independent of the session name (which can be renamed) and the claude
// session_id (which rotates on clear/compress), so it stays valid for the
// session's whole life.
func NewContainerName() (string, error) {
	return NewContainerNameWithPrefix(containerPrefix)
}

// NewContainerNameWithPrefix is NewContainerName under a caller-supplied name
// namespace. Tests use a unique prefix so their SandboxExecutor.List/reconcile
// can only ever see (and remove) their own containers, never a real session's.
func NewContainerNameWithPrefix(prefix string) (string, error) {
	id, err := NewSessionID()
	if err != nil {
		return "", err
	}
	return prefix + strings.ReplaceAll(id, "-", "")[:12], nil
}

// NewSessionID returns a random RFC-4122 v4 UUID for use with --session-id.
func NewSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// Package session drives headless AI backend turns and tracks sessions as
// durable records (a session_id on disk tied to a directory), not live
// processes. This is the data path for the voice interface: each dictated turn
// shells out to the session's backend CLI (Claude Code's `claude -p
// --output-format stream-json` by default — see internal/agent for the backend
// registry) and the clean reply text is returned for text-to-speech. See
// docs/protocol.md and the "TUI capture" decision in CLAUDE.md.
package session

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"sync"

	"github.com/bam/claude_spawner/server/internal/agent"
)

// newLineScanner is the JSONL scanner shared by the transcript/discover
// readers; it lives in the agent package alongside the backend stream parsers
// that also use it.
var newLineScanner = agent.NewLineScanner

// Session is a durable record. There is no long-lived process: the conversation
// state lives on disk under SessionID and is reattached via `claude --resume`.
type Session struct {
	// mu guards every mutable field below. The store hands the SAME *Session to
	// every caller (a running turn's goroutine, each device's read loop, the job
	// reconciler), so field writes are concurrent by construction: mutate only
	// inside Mutate, and read a consistent copy with Snapshot. It is a pointer,
	// lazily created by mutex(), so a plain &Session{...} literal (spawn, tests,
	// json.Unmarshal) stays valid and copying a record copies no lock.
	mu *sync.Mutex

	// seedPending, when non-nil, is an open promise that a PendingSeed is still
	// being computed for the CURRENT SessionID — see BeginSeed/AwaitSeed. It is
	// process-local (never persisted): a server restart simply loses the in-flight
	// recap, which is the same outcome as a failed read.
	seedPending chan struct{}

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
	// AliasIDs are ids this session was ADDRESSABLE by but never wrote a transcript
	// under: the placeholder id minted at spawn for a self-assigning backend
	// (opencode/codex/antigravity), displaced when the backend announced its own id
	// on the first turn. Clients that learned the placeholder (session list, the
	// spawn `attached` frame) keep using it, so it must still resolve to this record
	// — but it names no transcript, so it stays out of TranscriptIDs.
	AliasIDs []string `json:"alias_ids,omitempty"`
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
	// a later commit of the SSH-native epic (see TODO.toml); today nothing sets it.
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

// recordLockInit serializes the lazy creation of per-record mutexes. A record is
// born unshared (a literal, or a fresh unmarshal in OpenStore) and only later
// escapes to several goroutines via the store, so the one moment its mu pointer
// is written must itself be guarded — this global does that, and is held only for
// the pointer assignment, never across the record's critical section.
var recordLockInit sync.Mutex

// mutex returns this record's lock, creating it on first use.
func (s *Session) mutex() *sync.Mutex {
	recordLockInit.Lock()
	defer recordLockInit.Unlock()
	if s.mu == nil {
		s.mu = &sync.Mutex{}
	}
	return s.mu
}

// Mutate runs fn with this record's fields locked. EVERY write to a stored
// record's fields goes through it — a turn goroutine stamping Started/PendingSeed
// races a device's read loop changing the model or killing a job, and both race
// the store's flush marshalling the record. Group a multi-field change (e.g. a
// session_id rotation) into ONE Mutate so no observer sees the record half-rotated.
// fn must not call back into Mutate/Snapshot on the same record (the lock is not
// reentrant) or into Store.Put (which flushes, and flush takes record locks).
func (s *Session) Mutate(fn func(*Session)) {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	fn(s)
}

// Read runs fn with this record's fields locked, for a caller that needs a
// consistent read of a few fields without copying the whole record (Snapshot's
// job). Same rule as Mutate: fn must not re-enter Mutate/Read/Snapshot on this
// record, and must not do I/O — the lock is held throughout.
func (s *Session) Read(fn func(*Session)) {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	fn(s)
}

// Snapshot returns a deep copy of the record taken under its lock: a stable,
// unshared view safe to marshal or read field-by-field while other goroutines keep
// mutating the live record. Every slice is cloned, so an in-place append on the
// live record can't write through into the copy (which is what made Store.flush
// race with a PriorIDs/Jobs append). The copy carries no lock of its own — it is
// not a stored record and must not be handed back to the store.
func (s *Session) Snapshot() *Session {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	cp := *s
	cp.mu = nil
	cp.seedPending = nil // the promise belongs to the stored record, not to a copy
	cp.PriorIDs = append([]string(nil), s.PriorIDs...)
	cp.AliasIDs = append([]string(nil), s.AliasIDs...)
	cp.PendingNotes = append([]string(nil), s.PendingNotes...)
	cp.AgyBrainIDs = append([]string(nil), s.AgyBrainIDs...)
	cp.Jobs = append([]BackgroundJob(nil), s.Jobs...)
	cp.History = append([]HistorySegment(nil), s.History...)
	for i := range cp.History {
		cp.History[i].IDs = append([]string(nil), cp.History[i].IDs...)
	}
	return &cp
}

// MutateWithSeed applies fn like Mutate and, in the SAME critical section, opens a
// promise that a PendingSeed for the resulting context is still being computed
// off-thread; it returns the function that settles that promise. It exists so a
// context rotation (set_agent) can answer the client immediately while the
// expensive recap — a whole transcript chain read over SSH — runs in a goroutine.
// Rotating and promising atomically is the point: a turn that observes the fresh
// SessionID always also observes the pending promise, so AwaitSeed makes "the seed
// is late" a wait rather than a silent loss of the prior conversation.
//
// The returned settle function stamps the seed (an empty one settles the promise
// without touching the record) and closes it exactly once. It is a no-op if the
// SessionID rotated again in the meantime — a clear/compress/second switch means
// the recap describes a context the record has already left, and resurrecting it
// there would seed the wrong conversation. Call it exactly once, from the
// computing goroutine, on every path including error.
func (s *Session) MutateWithSeed(fn func(*Session)) (settle func(seed string)) {
	mu := s.mutex()
	mu.Lock()
	fn(s)
	if s.seedPending != nil { // an older promise is stranded; release its waiters
		close(s.seedPending)
	}
	ch := make(chan struct{})
	s.seedPending = ch
	forID := s.SessionID
	mu.Unlock()

	var once sync.Once
	return func(seed string) {
		once.Do(func() {
			mu.Lock()
			defer mu.Unlock()
			if s.seedPending != ch {
				return // superseded: whoever replaced this promise already closed it
			}
			s.seedPending = nil
			if seed != "" && s.SessionID == forID {
				s.PendingSeed = seed
			}
			close(ch)
		})
	}
}

// AwaitSeed waits for an in-flight BeginSeed promise to settle, so a caller about
// to read PendingSeed sees the finished value rather than a half-computed one. It
// returns immediately when no seed is pending (the common case), and gives up when
// ctx is done — the turn then proceeds without the recap rather than hanging.
func (s *Session) AwaitSeed(ctx context.Context) {
	mu := s.mutex()
	mu.Lock()
	ch := s.seedPending
	mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case <-ch:
	case <-ctx.Done():
	}
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
// under (or been addressable by): its current SessionID, an id retired by
// clear/compress (PriorIDs), a spawn placeholder displaced by a self-assigning
// backend (AliasIDs), or an id archived by a backend switch (History). Used to attribute a dir-keyed
// background job to the session that launched it — the job stamps the session_id
// current at launch, which may since have rotated, so a single-id check isn't
// enough.
// It takes the record's lock — like every other exported reader on a stored
// record — so a concurrent rotation can't be observed half-applied. Callers
// already inside Mutate/Read use ownsIDLocked.
func (s *Session) OwnsID(id string) bool {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	return s.ownsIDLocked(id)
}

// AdoptSessionID moves the session onto an id a self-assigning backend
// (opencode/codex/antigravity) announced in its stream, and marks it started so
// the next turn resumes rather than recreates it.
//
// The id being displaced doesn't just vanish: clients already hold it (the
// session list and the spawn `attached` frame carried it) and will attach by it,
// while the store re-indexes byID onto the new id on the next Put. So it is kept
// on the record and stays resolvable via OwnsID/GetByAnyID — otherwise a reattach
// after the first turn misses the registry and is refused as an unknown session.
// Which chain it joins depends on whether it ever ran: an unstarted spawn
// placeholder names no transcript (AliasIDs), an id that already took turns does
// (PriorIDs).
func (s *Session) AdoptSessionID(newID string) {
	if newID == "" {
		return
	}
	s.Mutate(func(r *Session) {
		if old := r.SessionID; old != "" && old != newID {
			if r.Started {
				r.PriorIDs = appendUnique(r.PriorIDs, old)
			} else {
				r.AliasIDs = appendUnique(r.AliasIDs, old)
			}
		}
		r.SessionID = newID
		r.Started = true
	})
}

// appendUnique appends id to the id chain unless it is already there (id chains
// are short and append-once, so a linear scan is the whole cost).
func appendUnique(ids []string, id string) []string {
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	return append(ids, id)
}

// ownsIDLocked is OwnsID's body, for callers that already hold the record lock.
func (s *Session) ownsIDLocked(id string) bool {
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
	for _, a := range s.AliasIDs {
		if a == id {
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
// Locks the record: PriorIDs and SessionID rotate together, and a caller must
// never see the new id alongside the old prior list (or vice versa).
func (s *Session) TranscriptIDs() []string {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	return s.transcriptIDsLocked()
}

// transcriptIDsLocked is TranscriptIDs' body, for lock-holding callers.
func (s *Session) transcriptIDsLocked() []string {
	ids := make([]string, 0, len(s.PriorIDs)+1)
	ids = append(ids, s.PriorIDs...)
	ids = append(ids, s.SessionID)
	return ids
}

// HasPriorID reports whether id is one of the session_ids this session retired via
// a "clear"/"compress" context rotation (see PriorIDs). It does NOT match the
// current SessionID — callers check that separately.
// Locks the record (a clear/compress appends to PriorIDs concurrently).
func (s *Session) HasPriorID(id string) bool {
	mu := s.mutex()
	mu.Lock()
	defer mu.Unlock()
	return s.hasPriorIDLocked(id)
}

// hasPriorIDLocked is HasPriorID's body, for lock-holding callers.
func (s *Session) hasPriorIDLocked(id string) bool {
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

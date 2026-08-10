# Architecture & internals

How claude_spawner works under the hood — the deep detail behind the one-line summary in
`CLAUDE.md`. Read this when you're changing the data path, the session driver, or transcription;
you don't need it for most turns. High-level "what it is" and the behavioral rules stay in
`CLAUDE.md`; user-facing setup/run and the narrative "how responses are captured" live in
`README.md`.

## Data flow

```
┌─────────────────────────┐         WebSocket          ┌──────────────────────────────┐
│   Android app (Kotlin)  │ ─── audio / control ─────> │        Server (Go)           │
│  - VAD-gated capture    │                            │  - WebSocket gateway         │
│    (streams speech up)  │ <── transcript / output ── │  - Whisper transcription     │
│  - audio capture        │                            │  - wake match + command FSM  │
│  - TTS playback         │                            │  - session driver + store    │
│  - session UI           │                            │                              │
└─────────────────────────┘                            └──────────────┬───────────────┘
                                                                       │ claude -p --resume <id>
                                                                       │ --output-format stream-json
                                                                       v
                                                        ┌──────────────────────────────┐
                                                        │ headless claude (per turn)   │
                                                        │  -> NDJSON: assistant / tool │
                                                        │     / result  (clean text)   │
                                                        │  state persists to disk via  │
                                                        │  session_id (no live proc)   │
                                                        └──────────────────────────────┘
                                  tmux is inspected only to detect a `claude` a
                                  human already has open in a pane (conflict warning)
```

- **Wake word**: matched **server-side, in the transcript** (`command.StripWake`) — there is no
  on-device keyword engine. The phone streams VAD-gated speech up; the server transcribes it and
  looks for the wake phrase (plus its mishearing variants and any custom wake tokens).
- **Transcription (STT)**: server-side Whisper (whisper.cpp or a local Whisper service). The app
  streams captured (VAD-gated) audio; the server returns a transcript and applies the wake/command
  grammar to it. **Repetition-loop guard** (`internal/transcribe`): Whisper hallucinates by
  looping a phrase on long/low-energy clips ("X. X. X. …"). Two mitigations, both in `transcribe`:
  the decoder runs with **no-context** (CLI `-nc`; remote `no_context=true`) so a window can't seed
  the next with its own hallucinated tail, and `clean()` runs `collapseRepeats()`, which drops
  back-to-back duplicate sentences and 3+ repeats of a short phrase before the text hits the
  wake/command seam.
- **Token detection (optional, `SPAWNER_WAKEWORD_URL`)**: a purpose-trained keyword-spotting sidecar
  (`spawner-wakeword`) can replace the *string-match* half of detection. When configured, `gatedChunk`
  (`internal/gateway/stream.go`) scores each hands-free clip's raw PCM through `internal/detect`
  (`RemoteWakeword` → sidecar `POST /detect`) for the end token instead of matching the fast
  transcript — that string-match is where Whisper's mishearings cause *missed* commits. It's a **gate,
  not a transcriber**: on a hit, `commitMessage` still re-transcribes the whole utterance with accurate
  Whisper for the real parse, so command text is unaffected. Thresholded in Go (`SPAWNER_WAKEWORD_THRESHOLD`);
  a nil/unreachable/erroring detector falls back to the Whisper string-match automatically (the A/B
  safety net). The audio source is unchanged — still bounded VAD/push-to-talk clips, no always-on
  stream (the sidecar left-pads short clips to its 2s window). The sidecar's `GET /stream` WebSocket
  exists for a possible future mid-word-latency path but is **not** used by the gateway today.
- **Speech gate (the pipeline's front door)**: when a client enables it (`hello.dictation_gate` plus
  a `speech_gate` token in the spoken-token catalogue), the gate phrase is the **entry condition for
  hands-free capture**, not a filter on the resulting text. `gatedChunk` runs closed until it fires:
  each clip goes to `openGate` (`internal/gateway/stream.go`) and is scored — by the same detector
  path as the end token when the token carries a model, otherwise by the tiny fast pass used purely
  as a matcher — and then **discarded whole**. Nothing is appended to `audioPCM` or the draft buffer,
  no `pending`/`transcript` is sent, and `commitMessage`'s accurate pass never runs, so ambient
  chatter costs at most one tiny pass (zero, with a detector) and is invisible to the app. On a hit
  the gate opens and the clip that opened it is kept **whole** — it also holds the first words spoken
  after the phrase — with `stripGate` trimming the pre-gate tail at the *text* level, so the audio
  boundary can be sloppy while the message stays exact. `stripGate` runs on the **joined draft**, not
  just at commit, which is the load-bearing detail: every downstream matcher (end token, wake word,
  dictation) sees text with the opening bracket already gone, so **one phrase can serve as both
  brackets** — "pickle … pickle" opens on the first occurrence and ends on the second. `commitMessage`
  strips in the same order (gate, then end token). The exception is a *shared detector model* bound to
  both actions: an acoustic hit can't be attributed to one bracket, so `sharesModel` suppresses the
  end-token score on the clip that just opened the gate. Every commit re-closes the gate, as do
  `clearBuffer` and a `gateIdleTimeout` (90s) that stops a detector false positive from leaving
  capture live. There are **no ungated paths** — commands and barge-in `hey buddy stop` are gated
  like dictation, because a front door with a side entrance isn't one.
- **Transport**: a single WebSocket per app session carries audio up and transcripts/session
  output down. Use REST only for stateless control actions if needed.
- **Session control**: the server shells out to `claude` headless (see below). Input is the prompt
  arg; output is parsed from `stream-json`. tmux is not on the data path — it is inspected only to
  notice a `claude` a human already has open interactively.

The **text seam**: the app sends an `utterance` message with already-transcribed text. The audio
path (`wake` → binary PCM16 frames → `audio_end`) assembles a WAV, runs the Transcriber, emits a
`transcript`, then feeds the text through that exact same seam — so the command/dialog/turn
machinery is engine-agnostic and was fully exercised before STT existed.

## ✅ RESOLVED: how we capture Claude's responses (do NOT scrape the TUI)

The original worry was that Claude Code in tmux is a full-screen TUI (ANSI, redraws, spinners),
so reading its output for TTS looked painful. **We do not scrape the TUI at all.** Decision,
validated end-to-end against `claude` 2.1.196:

> Drive Claude Code **headless** in `stream-json` mode. A "session" is a durable
> **`session_id` on disk tied to a directory**, not a live process. Each dictated turn shells
> out to `claude`, and the clean `result` event is the text we speak.

Per-turn invocation (working dir = the session's directory):

```
claude -p "<transcribed text>" \
  --session-id <uuid>      # FIRST turn: we generate the uuid ourselves
  # --resume <uuid>        # LATER turns: reattach instead of --session-id
  --output-format stream-json --verbose \
  --dangerously-skip-permissions
```

Parsing stdout (newline-delimited JSON):
- `type:"system"` (init), `type:"assistant"`, `type:"user"` (tool results), `type:"rate_limit_event"` — ignore for TTS.
- `event.type:"content_block_start"` with `content_block.type:"tool_use"` → optional spoken
  breadcrumb ("running Bash…"), using `content_block.name`.
- **`type:"result"`** → `result` is the clean final answer to speak; `session_id` confirms the id;
  `subtype` is `"success"` or `"error_*"` (treat non-success / `is_error` as a failed turn).

For TTS we take the **final `result`**, not token deltas — TTS wants whole sentences.
`--include-partial-messages` (requires `--verbose`) gives `text_delta` events if we later want
live on-screen streaming, but it is not needed for the voice path.

This is implemented in `internal/session` (`Driver.Turn`, `Store`, `NewSessionID`) and was
verified: turn 1 with `--session-id` then turn 2 with `--resume` correctly retained context.

### tmux is used only to detect a live interactive `claude`

Because the session is a `session_id` on disk, a human could also `claude --resume <id>` it in a
terminal. `internal/tmux` exposes just `ClaudeDirs` — the set of directories with an interactive
`claude` open in a pane — so the spawner can warn before driving that same session headlessly.
**One active writer per session at a time** — don't run a headless turn against a `session_id` a
human is editing live. (An earlier design had the server itself open a "babysit" pane via a
`Babysit`/`List`/`Exists`/`Close` API; that was dropped — the server never creates panes now.)

### Record locking: mutate a `Session` only through `Mutate`

The store hands out **shared `*Session` pointers** — the same record is held by a running turn's
goroutine, by every attached device's read loop, and by the background-job reconciler — so field
writes are concurrent by construction (a turn stamping `Started`/`PendingSeed` against another
device's `set_agent`/`set_model`/kill-job). `Store`'s own mutex protects only the name/id **index
maps**, not the records' fields.

So each record carries its own lock (`Session.mu`, lazily created) and three seams:

- **`Session.Mutate(fn)`** — the only sanctioned way to write a stored record's fields. Group a
  multi-field change (a `clear`/`compress`/`set_agent` session_id rotation) into **one** `Mutate`
  so no observer sees the record half-rotated. Not reentrant; do no I/O and don't call `Store.Put`
  inside it.
- **`Session.Read(fn)`** — a consistent read of a few fields (used by `Driver.ProfileFor`, which
  runs on the turn path while `Store.Rename` may be rewriting `Name`).
- **`Session.Snapshot()`** — a deep copy taken under the lock, every slice cloned. `Store.flush`
  marshals **snapshots**, never the live pointers: encoding a shared record raced any in-place
  `PriorIDs`/`Jobs` append. A reflection-driven test (`record_lock_test.go`) fails if a newly added
  slice field isn't cloned, so the invariant can't quietly rot.

Related: per-launch scratch does **not** live on the record. The resolved `*ExecProfile` is an
explicit parameter of `Executor.Start`/`SandboxLifecycle.Ensure` — it used to be stashed on the
shared `Session`, where a turn and the job reconciler launching at once overwrote each other's
profile. `go test -race ./...` is the check that keeps all of this honest.

**Reads follow the same rule.** A single-field read off a live shared record is a data race too, so
nothing reads a stored record's fields directly:

- **Pure readers snapshot at the boundary.** A function that only reads a record takes
  `rec.Snapshot()` once at entry and works off that — `Driver.Turn` (which then hands the snapshot
  to the executor), `ReadDisplayHistory`/`DisplayDigest`/`ArchiveSegment`/`DeleteSessionAll`,
  `RunOnTarget`, `msgAttached`, the session/discovered list builders, and the off-loop tickers
  (`jobReconcileLoop`, `autoCompressLoop`, `reconcileJobs`). One snapshot also means the fields
  *agree with each other*: an agent, its host and its id chain all describe one moment.
- **One-field reads use `recName(rec)` / `recID(rec)`** (gateway) — the locked one-liners for the
  many places that just want a name or a session_id.
- **A turn's identity is read once**, before the goroutine starts: `startTurn`/`startCompress`/
  `startJobNotify` capture name + session_id under the lock and use those for the turn's whole life,
  rather than re-reading a record they themselves rotate.
- **The record's own helpers lock** — `OwnsID`, `TranscriptIDs`, `HasPriorID` (with unexported
  `…Locked` bodies for callers already inside `Mutate`/`Read`).

`TestStoreReadersRaceRotation` in `record_lock_test.go` drives a rename and an id rotation against
those readers concurrently, so an unlocked read reintroduced later fails `go test -race ./...`.

This is the field-level complement to the "one active writer per session" rule above, not a
replacement for it.

## Per-session execution target (host vs sandbox)

Status: **implemented** (`internal/session/executor.go`). Goal: let each spawned Claude session run
*either* directly on the host (real host files/toolchains) *or* inside an isolated container sandbox
(disposable, root-inside-the-sandbox) — chosen **per session** via `Session.Target`.

### The single seam

Every turn already funnels through one place: `session.Driver.Turn()` (`internal/session/
session.go`), which `exec`s the `claude` binary in the session's `Dir` and parses its
`stream-json` stdout. Nothing else in the server knows *how* that process is launched. So the whole
feature reduces to making that launch pluggable:

- An **`Executor`** interface (start a `claude` turn given `Dir` + args, return a stdout stream).
  The direct-`exec` `HostExecutor` is now **test-only** — in production the `host` target uses
  `SSHExecutor` (SSH-native execution is unconditional; see below). A `sandbox` executor
  (`SandboxExecutor`) runs the turn inside a container. `Turn()` selects one and is otherwise
  unchanged — the NDJSON parsing, `Setpgid` group-kill, and event fan-out all stay put.
  Every `Proc` also exposes **`Stderr()`**, a bounded tail of what the backend wrote to stderr.
  That is where the actionable failure cause lives when stdout ends without a usable event
  ("model not found", "connection refused", an auth error), so `Turn()` appends it to the turn
  error — which the gateway sends as `turn_failed` and the app shows in chat. Without it the user
  only ever saw the useless "stream ended without a text message".
- An **execution-target field** on the `Session` record (`store.go`), set at spawn time and
  persisted in `sessions.json`, so host-vs-sandbox is a durable per-session property the spawn
  dialog chooses. Default = `host`.

## AI backend registry (which AI — orthogonal to where it runs)

Status: **implemented** (`internal/agent`). The `Executor` seam above answers *where* a turn runs
(host / sandbox / SSH). A second, orthogonal seam answers *which AI* runs it and *how* to invoke
and parse it — so the server drives more than `claude`.

- An **`Agent`** (`internal/agent`) is a **self-contained** headless backend, one file per backend
  (`claude.go`, `codex.go`, `opencode.go`): an id (persisted on the session), a `Bin` (the command to launch), a
  `DefaultModel`, a catalogue of selectable `Models` (each by a short spoken alias —
  `opus`/`sonnet`/`fable`/`haiku` plus the `opus-low`/`opus-high`/`opus-max` `--effort` presets, or
  Codex's reasoning presets). The `Models` slice is the compiled
  **fallback**: a backend may also declare **live discovery** (`DiscoverArgs` + `ParseModels`) —
  a command whose stdout lists the models it can *currently* run — and when a probe succeeds the
  discovered catalogue **shadows** `Models` everywhere it's read (`Agent.Catalog`, guarded for the
  runtime refresh). So a backend fronting an external model store (opencode → Ollama or Zen) reports its
  real list with no rebuild, while backends with a fixed set (Claude, Codex) just carry `Models`. It
  also has a per-backend **arg builder**
  (`Agent.Args(TurnSpec)`) that emits that backend's exact command line, its own **stream parser**
  (`Agent.ParseTurn`, normalizing the backend's output to the shared `TurnResult` — reply, usage,
  self-assigned session id), and a declared **transcript layout** (`Agent.Transcript`). The
  backend-neutral turn vocabulary (`ToolUse`/`Usage`/`RateLimit`, `TurnCallbacks`, `TurnResult`)
  lives in `agent/turn.go`. The `Registry` holds the known agents; an empty/unknown id resolves to
  the default (Claude), so records predating the field just run on Claude.
- `Session` gains a durable **`Agent`** (backend id) and **`Model`** (alias). `Driver.Turn` resolves
  the agent, asks it to build the args, passes the resolved backend binary to the `Executor`
  (`Driver.binFor` — empty defers to the executor's own `SPAWNER_*_CLAUDE_BIN`, keeping Claude
  unchanged), and hands the stream to the agent's own `ParseTurn`. **`Turn` contains no per-backend
  branching** — the only conditionals are on declarative Agent fields (`SelfAssignsID`,
  `Transcript`), never on which backend it is.
- **Backend × target is a matrix, not a special case.** Because *which AI* and *where* are separate
  seams, any backend runs on any target: the arg builder never mentions host/sandbox/SSH, and the
  Executor never mentions claude/codex. Adding a backend touches neither the executors nor the
  gateway.

**Four backends ship today.** *Claude* (`--output-format stream-json`; the server mints the
`session_id` and passes `--session-id`/`--resume`). *Codex* (`codex exec` / `codex exec resume`,
`--json` JSONL): Codex **mints its own** session id (`thread_id`, read from the first output event),
so `Agent.SelfAssignsID` tells `Turn` to adopt the id `ParseTurn` returns in
`TurnResult.SessionID` rather than supplying one. Adoption goes through
`Session.AdoptSessionID`, which **keeps the displaced id on the record** — the spawn already
handed that placeholder to the app (session list, `attached`), so if it stopped resolving, the
first reattach after the first turn would miss the registry and be refused as an unknown session.
An unstarted placeholder joins `AliasIDs` (addressable, but names no transcript, so it stays out of
`TranscriptIDs`); an id that already ran turns joins `PriorIDs`. Model availability
can be **plan-dependent** (on a ChatGPT-account Codex, only `gpt-5.5` is `-m`-selectable, so its
alternates are reasoning-effort presets); the registry is the single place that catalogue lives.
*Ollama* and *Zen* are both opencode-backed (`opencode run` / `run -s <id>`, `--format json` JSONL):
like Codex they **self-assign** a session id (a `ses_…` id on every event), and `--auto` is the
skip-permissions equivalent. Ollama advertises the local `ollama/*` catalogue served by the provider
block in the host user's `~/.config/opencode/opencode.jsonc` (pointed at the local Ollama server).
Zen advertises OpenCode Zen's subscription catalogue, whose opencode model ids use the `opencode/*`
prefix. Both use **live model discovery**: `DiscoverArgs` runs `opencode models ollama` or
`opencode models opencode` on the host and `ParseModels` strips the provider prefix into the model
alias, so whatever opencode is configured to run appears in the app automatically. Pulling or adding
models and wiring them into opencode remains the user's job — the server treats opencode as the
source of truth for what's runnable. `Driver.RefreshModels` runs the probes over the SSH pool at boot
(before the provider overlay is validated) and, throttled, on each client connect. opencode persists
sessions in a SQLite DB rather than flat files, so its transcript reader shells out to opencode's own
commands (see below). That reader also **unquotes user messages**: opencode stores a prompt passed
as a CLI argument wrapped in double quotes and exports it that way, and those two bytes make the
replayed text differ from what was sent — enough that the gateway's `stripInjected` misses its own
trailing scaffolding and the app can no longer match the replayed row to the live one, so the
message appears twice. It's undone at the reader, so every consumer sees the user's actual text.
The older persisted/spoken `opencode` backend id remains an alias for Ollama.
*Antigravity* (Google's Gemini-powered `agy` CLI) is driven with
`agy --prompt` (non-interactive "print" mode) plus **`--output-format stream-json`** — a flag absent
from `agy --help` but real (the binary's own flag table names `text, json, stream-json`), which turns
the turn into a newline-delimited event stream like every other backend's. `parseAgyStream` reads it:
an `init` event carrying the conversation id, `step_update` events (`agent_response` text arriving as
`text_delta` chunks to accumulate per `step_index`, `tool` steps giving live breadcrumbs, each step
reported `ACTIVE` then `DONE`), and a final `result` event holding the authoritative reply with its
paragraph breaks intact, plus the turn's summed **token usage** (cache reads only — agy reports no
cache-write count, so no cache-warm signal). It **mints its own conversation id** (`--conversation`
can only *resume* an id agy created — a caller-supplied uuid is rejected with "not found, ignoring"),
so `SelfAssignsID` is true and the first turn runs with no `--conversation`, adopting the id the
`init` event announces; later turns resume with it, which is what gives agy sessions cross-turn
memory. Unlike every other backend it **ignores the process cwd** — it works in its own scratch
project unless the workspace is named, so `Turn` threads the session directory through `TurnSpec.Dir`
and the build passes `--add-dir`. That conversation id is also the name of agy's on-disk *brain*
directory, so it doubles as the history key: `Turn` records it in `Session.AgyBrainIDs` (normally one
entry; a session grows a second only if a resume failed and agy minted a fresh conversation) and
`antigravityFS` (`internal/session/antigravity_transcript.go`) replays that chain on reattach — one
user row + one joined assistant row per brain transcript.

**Reattach replays each backend's own on-disk transcript.** A session has no live process, so the
`history` page and the on-attach context badge are rebuilt from disk — and *where* that record lives
and *how* it's shaped differs by backend, so the reader is chosen by the agent's declared
`Transcript` layout (`Driver.transcriptReaderFor`). Claude writes
`~/.claude/projects/*/<session_id>.jsonl` (read by `claudeFS`); Codex writes a **rollout** JSONL at
`~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread_id>.jsonl` in an unrelated schema — conversation
prose as `event_msg` `user_message`/`agent_message` lines, context size as `token_count` lines — read
by `codexFS` (`internal/session/codex_transcript.go`). opencode keeps sessions in a **SQLite database**
(`~/.local/share/opencode/opencode.db`), not files, so `opencodeFS`
(`internal/session/opencode_transcript.go`) instead shells out to opencode's own stable commands over
the same SSH seam — `opencode export <id>` for history (mapping its message/part JSON, taking each
turn's context size from the last `step-finish` part's tokens, since the session-level `info.tokens`
is summed across turns) and `opencode session delete <id>` for removal. It also **unwraps the double
quotes opencode puts around a user message** it took as a CLI argument: those two bytes aren't the
user's words, and every exact-text consumer downstream breaks on them — the gateway's `stripInjected`
stops recognizing its own trailing scaffolding, and the app can no longer match the replayed row
against the live one, so the message shows twice with the "(Reply briefly…)" hint attached. Antigravity keys its store by
*brain-dir* ids — which are the conversation ids it announces, recorded per session in
`Session.AgyBrainIDs` — so `antigravityFS` replays those brain transcripts, each brain's
`transcript.jsonl` becoming one user row (unwrapped from agy's `<USER_REQUEST>` envelope) plus one
joined assistant row (its `PLANNER_RESPONSE` steps). It deletes nothing (the brain dirs are agy's own
store) and reports no context snapshot: agy's usage is per-turn totals in the live stream, not a
window size on disk. All four normalize to the same `[]Message` / `ContextSnapshot` the gateway already sends, so a
Codex, opencode, or antigravity session's past turns replay on reattach much like a Claude session's.
(These persisted records are *not* the live `--json` streams the agents' `ParseTurn` consume during a
turn.)

**Transcript reads are bounded and incremental, because these files get huge.** A real
`~/.claude/projects` here is ~900 MB, with single transcripts of 20–26 MB (sometimes only a few
hundred lines — one tool result can be megabytes), and every read of a *remote* session goes over
SSH. Three rules keep attach and inter-turn latency off that cliff, all enforced at the `claudeFS`
seam rather than at the call sites:

- **Read the end, not the file.** The context badge (`lastContextUsage`) needs only the newest
  usage-bearing assistant line, so it scans *backward* over a bounded tail (`tailBytes`, widening
  only if a run of giant tool-result lines fills the window) — the mirror of the bounded *head* read
  `cwds` uses for the working directory. This runs at the end of every turn, on a file the turn just
  grew, so an *in-process* cache keyed on (size, mtime) can never help it — which is why the snapshot
  is also memoized **durably** by chain signature (`UsageCache`, `state/usage.json`, alongside
  `DigestCache`). Attach blocks its `attached` ack on this lookup, and the post-turn attach is exactly
  the case that always missed; a session whose transcripts haven't moved since the snapshot was taken
  now answers from a stat. Callers holding a `*Session` go through `Driver.SessionContextUsage`;
  `LastContextUsage` (raw, uncached) remains for the id-only callers.
- **The durable caches never write on the caller's thread.** `DigestCache` and `UsageCache` share one
  mechanism (`jsonStore`): a `Put` only edits the in-memory map and bumps a version, and a single
  background writer coalesces versions into at most one whole-file, atomically-renamed write per
  debounce window. That matters because a `Put` used to marshal the entire map and do a
  CreateTemp+write+rename *under the mutex* — the 8-way digest sweep serialized behind N whole-file
  rewrites, and every turn end paid one. Since exactly one goroutine writes the file, snapshots land
  in version order, so the lost-update bug that forced the old under-the-lock write can't recur.
  Losing the last few milliseconds of entries to a crash is fine: both caches are accelerators whose
  entries recompute. `Sync()` (tests only) blocks until what's been recorded is on disk.
- **Parse only what was appended.** These transcripts are append-only, so the message parse is
  *resumable* (`claudeParse`): messages so far, the byte offset they cover, and the still-open
  dictation's turn rollup. A later read re-reads only the new bytes — plus a small overlap of the
  already-parsed region, compared byte-for-byte, so "append-only" is **verified**, not assumed; a
  rewritten or truncated file falls back to a full re-parse. All of that — statting, choosing between
  an exact hit, an extension and a re-parse, the overlap proof, the cache — is generic and lives once
  in `readIncremental`; a backend supplies only an `incParse` (how one line folds into state, how that
  state renders to messages). **Codex** rollouts are the same shape and go through the same seam
  (`codexParse`), carrying the mid-scan `lastClaude` across appends because Codex writes a turn's
  `token_count` and durable `msg_…` id on lines *after* the message they badge. The older
  all-or-nothing (size, mtime) cache remains for the context snapshot.
- **Backends without a per-session file memoize the whole read instead.** *opencode* keeps sessions in
  one SQLite store and history comes from an `opencode export <id>` subprocess, so there is nothing to
  stat per session and nothing to extend: exports are memoized per id, keyed on the store's signature
  (`storeSig`). Crucially, every id in a chain but the last has been rotated away and can never gain
  messages, so those hits stand however much the store moves — an unrelated session's turn re-exports
  the live id only, not the whole chain. *Antigravity* needs neither: one brain is one turn, and an
  archived brain's transcript never changes, so the stat-keyed parse cache already never misses on it.
- **Resolve a session id to its path once.** The project-dir encoding follows the working directory,
  so a transcript's path is fixed for its lifetime; the remote lookup is memoized per host, and the
  entry is dropped whenever a resolved path stops working, so a move or delete self-heals.

The same reasoning drives the callers: a `history` request that carries the hash the app already
holds is answered from `DisplayDigest` (a few stats) *before* any full read, because gateway handlers
are dispatched serially off one inbound loop — a needless multi-megabyte parse there blocks every
other request on the connection.

For the same reason, the handlers whose work is a *blocking round trip* don't run on that loop at
all: history, the transcript-digest sweep, discover, and the picker's directory `browse` each hand
off to a goroutine with a small per-connection semaphore, replying through the usual serialized
writer. `browse` in particular is one SSH round trip per tap, so serving it inline stalled the turn
the user was speaking behind a folder tap; identical in-flight listings are dropped rather than
queued, since a listing is a read-only snapshot and the reply already on its way answers both. The
app closes the other half: both controllers put their browse request/reply pair through the shared
`ListingCache` (an LRU keyed by host + path + files), so a tapped or "up" directory paints from cache
instantly while the request still goes out and its reply refreshes the view.

A request that *does* need bodies goes through `Driver.DisplayHistory`, which stats the chain **once**
for both the log and its digest and serves what it can from the in-memory display memo
(`displaymemo.go`), at two granularities. The whole assembled log is memoized per session under the
chain's signature, so paging older messages — `before != nil` skips the fast path by definition — no
longer re-parses the entire cross-backend chain per page. Each *archived* `HistorySegment` is memoized
under its own signature, and an archived segment belongs to a backend the session has switched away
from, so its signature never changes again: a session that keeps producing output misses the
whole-log memo on every turn yet still never re-reads its archives. Both levels hand out copies
(`serveHistory` strips injected scaffolding in place), and a segment whose read failed suppresses the
whole-log memo so a transient error can't be cached as a short log.

**A turn *in flight* isn't on disk yet — so mid-turn (re)attach is caught up from a live replay
buffer, not history.** A turn's streamed prose is only written to the on-disk transcript when it
finishes, so a device that attaches or reconnects *while* a turn is running can't recover the
already-streamed steps via a `history` refetch. The `sessionJob` therefore buffers the current turn's
`output` frames (`turnFrames`, reset each `beginTurn`, capped at `maxTurnFrames`) and `bindJob` replays
them to the freshly-bound connection (`replayInFlight`) before the "still working" breadcrumb — so a
long agentic turn's middle isn't lost to a reconnect. The turn-terminal reply is separately protected
by the per-connection `pending`/`orphan` redelivery buffers; the client dedups any replay overlap and
the whole reply collapses into the indexed history row once the turn lands.

**Frame attribution is one resolver, two backends.** Every server→client frame that belongs to a
session must carry its `session_id` (and, for turn frames, a `turn` id), or a client viewing a
*different* session misfiles it under whatever it's showing. That metadata is written in exactly one
place — `attribute(m, session, turn, overwrite)` in `attribution.go` — which both send backends funnel
through: the **hub fan-out** (`conn.jobSink`) attributes each fanned frame to the job's session,
authoritatively, so it tracks a compress rotation; a **direct unicast** (`conn.stampSession`, at the
`conn.send` write choke point) attributes a session-scoped notice to the connection's *current* session
(`currentSessionID` = attached, or none), filling only when the builder didn't already stamp it. Which
directly-sent types are session-scoped is the declared `sessionScoped` registry, not an inline check,
so a new such type can't silently ship unattributed. Builders that already bake in `session_id`
(`transcript`, `attached`, `context_reset`, `renamed`, `history`) attribute at construction and pass
through both backends untouched. Delivery stays two transports — plain unicast vs the hub's
fan-out + buffering + replay — but they speak this one attribution vocabulary rather than each
re-deriving the ids.

### Adding an AI backend (e.g. Gemini CLI, a local model)

The checklist, in dependency order — the design goal is that a new backend is **one new file plus
wiring**, and nothing in the gateway, executors, or clients changes:

1. **`internal/agent/<backend>.go`** — the whole backend in one file, `claude.go` as the template:
   a constructor returning an `*Agent` (id, name, `Bin`, models + default, `SelfAssignsID`,
   `Transcript`), its `build` func (the exact CLI for first-turn / resume / bypass / model), and
   its `ParseTurn` (stream → `TurnResult`, fanning live events out via `TurnCallbacks`). Add parser
   tests beside it (`parse_test.go` has the pattern, with real captured event shapes).
   - **Optional — live model discovery.** If the backend fronts an external, user-managed model set
     (like opencode → Ollama or Zen) rather than a fixed catalogue, declare `DiscoverArgs` (the argv whose
     stdout lists runnable models) and `ParseModels` (stdout → `[]Model`). `Driver.RefreshModels`
     runs it over the host SSH pool and installs the result via `Agent.SetDiscovered`; `Models` stays
     as the fallback when a probe fails. Keep discovered aliases in the same scheme as the fallback
     so a stored provider-overlay default/voice choice survives either path.
2. **Register it** in `agent.Default()`.
3. **Transcript reader** — if the backend's on-disk history layout isn't Claude-shaped, add a
   `TranscriptKind` constant and a reader in `internal/session` (see `codex_transcript.go`), and
   teach `transcriptReaderFor` the new kind. If it never persists transcripts, declare
   `TranscriptClaude` and reattach simply replays nothing.
4. **Binaries per target** — env vars in `internal/config` (host + sandbox, following
   `SPAWNER_SSH_CODEX_BIN` / `SPAWNER_SANDBOX_CODEX_BIN`), wired into `Driver.AgentBins` in
   `main.go`. Document them in `CLAUDE.md` (the docsync test enforces this).
5. **Voice spawn vocabulary** — add the backend's spoken name to `spawnAgentWords` in
   `internal/command` so "spawn a <backend> session" works (the visual picker needs nothing: the
   `agents` message advertises the registry dynamically).
6. **Docs** — update the backend list here; `docs/protocol.md` and the clients need no changes.

### The server runs in a container, driving the host over SSH (no broker)

The server runs in a **Docker container** that builds the Go binary from source — the one supported
deployment. It runs as the ordinary user (never host root) and reaches the host over **SSH**
(unconditional — SSH-native is not a toggle): it runs `claude` for host turns and drives the
rootless runtime for sandbox turns **on the host** over that same SSH connection, and reads every
session's Claude transcript back over it. There is no spawn-directory jail — a session may launch
anywhere on the target host. No component holds host root: the server is an unprivileged
container and sandboxes use a rootless runtime on the host. Recipe: the root `docker-compose.yml`
(the `spawner-server` gateway + the `wakeword` detector; STT/TTS containers live in the separate
`/data/speech_services` stack on the same ports; host networking so `localhost:22` is the host sshd
and `localhost:8572` reaches whisper; only durable state and the whisper models dir are mounted —
discovery, browse, and transcript reads all run on the host over SSH, not off a host home/root
mount). See the Dockerfile at `server/Dockerfile`.

> **Design note — the containerized-server + broker detour (reverted 2026-07-06).** An earlier design
> ran the server in a container and put a small host-side **broker** daemon (`cmd/broker`, dialed
> over a Unix socket) in front of the same `HostExecutor`/`SandboxExecutor` code, so the unprivileged
> container could reach the host without host root. It worked, but bought little: the broker itself
> ran bare metal, and the server never needed root, so the container protected the host from almost
> nothing while adding an IPC hop and a whole wire protocol to maintain. Don't re-introduce *that* (a
> bespoke Unix-socket broker); the privileged shortcuts — a `--privileged` server with `--pid=host` +
> `nsenter` — were rejected for the same "no component holds host root" reason and remain rejected.
> The container reaches the host over **standard SSH** instead (2026-07-08): `claude` runs on the
> host, no host root, no privileged shortcuts, no IPC protocol to maintain — the thing the broker
> detour was trying to buy, now bought by SSH. (There was a bare-metal-binary interregnum between the
> revert and the SSH-native container; it's gone now — the container is the only route.)

### SSH-native execution: the host is a dimension, localhost is just another host

The `host` target is served by the **`SSHExecutor`**: every host turn — the local machine
included — runs over SSH (SSH-native is unconditional; the direct-`exec` `HostExecutor` survives
only as the hermetic unit-test executor, never in the running server). A
per-host `SSHPool` (`internal/session/ssh.go`) dials + authenticates once and keeps the connection
alive, opening a cheap channel per turn. Which machine a session runs on is a durable per-session
field, **`Session.Host`** — orthogonal to the host/sandbox target. The **app owns the host
registry** (Settings → Hosts, persisted server-side as `hosts.json`); `Session.Host` names an entry
there, or a bare hostname the pool dials literally with the `SPAWNER_SSH_*` defaults.

Dialing is guarded by a **negative dial cache** in the pool. A failed dial marks that host's pool
entry down for a backoff window (30 s, doubling per consecutive failure to a 5-minute cap, reset on
the first success); every caller inside the window gets an immediate error naming the host and the
retry window instead of paying another full dial timeout — serialized, at that, on the host's entry
lock. This is what keeps one unreachable machine from turning a digest sweep, a chainSig batch, or a
context-usage read into minutes of stall: a dead host costs microseconds per call, not 15 s.

A host that is merely **slow** (dialable, but crawling) is handled the other way round: the whole
transcript read path — `Driver.DisplayDigest`/`DisplayHistory`, the `transcriptReader` interface,
and `claudeFS`'s SSH probes down to `SSHPool.Run` — takes a `context.Context`, so a caller's
deadline reaches the remote command instead of being wrapped around a blocking call. The
connect-time **digest sweep** uses that: each session gets its own deadline (8 s) and the frame is
sent with whatever answered. A session that misses it is simply omitted, which the app already
treats as "keep the cached transcript", and the next sweep retries it — so one slow box no longer
gates every other session's cache validation.

The pool also answers the question **non-blockingly**: `SSHPool.Down(host)` reports whether a host
is inside that window without ever waiting on the entry lock (a dial in flight answers "unknown"),
so work that should *degrade* rather than hang can ask first. **Session delete** is the case that
needs it. Purging a session's transcripts walks its whole chain — several remote commands per id,
per backend segment — so deleting a session on an offline box used to appear hung, and it was
exactly the session the user most wanted gone. The two halves are now split at the seam: the
registry record is dropped immediately, and any segment whose host is down becomes an owed item in
the durable **`PurgeQueue`** (`state/purges.json`, alongside `digests.json`/`usage.json`) — one
entry per (agent, host, id-set), deduped, surviving restarts. `purgeRetryLoop` (gateway, every
6 min) retries each owed purge once its host answers again and drops it when it succeeds, so the
remote cleanup is deferred, never leaked.

`Session.Host` is **always an explicit name** — there is no implicit "empty means localhost"
default. The loopback machine is the host name **`localhost`** (`session.LocalHost`), handled
exactly like any other SSH host (dialed over loopback SSH with the config defaults). It is **not a
special built-in**: `OpenHostStore` seeds a `localhost` entry into a *fresh* registry so a new
deployment lists it out of the box, but it is an ordinary row — editable and deletable like any
other (once the file exists it never re-seeds, so a delete sticks). The one place a default is
applied is at spawn time (`newSession`): a host-target session with no named host is set to
`localhost` so voice/legacy spawns keep working. Everywhere downstream — the executor, transcript
access (`claudeFS`), discovery — treats a hostless host-target session as a bug: the `SSHExecutor`
returns an error rather than silently running it on the local box. This is what makes a
**remote-only deployment** possible — delete the `localhost` host and the server drives only remote
machines, never touching its own box. (Legacy `sessions.json` records with an empty host are
migrated to `localhost` on load; discovered sessions, found by scanning this machine, are named
`localhost`.)

**What `localhost` means depends on the server's network namespace.** In a container it's the
container's own loopback — which has no sshd — *unless* the container shares the host's network. The
`spawner-server` service in the root `docker-compose.yml` uses **host networking** precisely so that
`localhost:22` inside the container is the **host's** sshd: the seeded
`localhost` host then drives the host machine over SSH (there is no host home/root mount — all of it,
including transcript reads and discovery, goes over that SSH connection). A container *without* host
networking can't reach the host as `localhost` — that's a
deployment where you'd delete the `localhost` entry and register the host (and any others) as
explicit remotes instead.

### Re-authenticating a host: `claude auth login` needs a PTY (`internal/session/authlogin.go`)

Logging a host's `claude` back in is the one place we run the CLI **under a PTY**, and it is a
deliberate exception to the no-PTY rule the turn path follows (`cancelableCommand` requests none so
stream-json stdout stays clean). The reason is measured, not stylistic: `claude auth login` reads its
"Paste code here if prompted >" from the terminal, so with plain pipes the process ignores whatever
is written to stdin and hangs forever. The full probe — subcommands, output shape, exit codes — is in
[`docs/scopes/claude-auth-login.md`](scopes/claude-auth-login.md).

`AuthLogin` is the supervisor. Its lifecycle is linear and human-paced: `StartAuthLogin` opens a
long-lived SSH channel, requests a `dumb` PTY with echo off, and runs `cd "$HOME" && exec claude auth
login --claudeai|--console`; `URL` blocks until the OAuth URL appears; `SubmitCode` writes exactly one
pasted code plus a carriage return; `Wait` returns the verdict (exit 0 = authenticated, otherwise the
transcript's last line, which is the CLI's `Login failed: …`). Three consequences worth keeping:

- **Which identity gets logged in is decided by `$HOME` on the target host**, nothing else — so
  picking the identity means picking the host entry, not overriding env here.
- **The URL is bound to that process.** Each invocation mints a fresh `code_challenge`, so the
  process must stay alive from URL to code, and starting a second login invalidates the first URL.
- **Nothing local is involved in the callback** — the `redirect_uri` is a hosted page, so the browser
  completing the flow can be the user's phone. No port is opened, no listener runs.

Under a PTY the terminal merges stderr into stdout and wraps the URL in an OSC-8 hyperlink (so it
appears twice, surrounded by escape bytes); the driver strips ANSI/OSC escapes before matching and
publishes the first URL only. Because stderr is merged there is no room for the pgid sentinel the
turn path uses, so cancellation is a SIGKILL plus channel close (`setsid` is *not* used — it would
detach the controlling terminal and break the prompt). Every attempt is bounded by
`DefaultAuthLoginTimeout`, since the CLI itself would wait at the paste prompt forever.

### Timers never read the host: event-driven refresh, cached reads

Because every read is an SSH round trip drawing from a per-host channel budget the live turn stream
also needs, **no periodic loop may initiate host I/O**. Recurring scans consume the last *known*
value and are refreshed by the events that can actually change it. The auto-compress monitor
(`gateway/autocompress.go`) is the canonical case: it ticks every few seconds over every started
session but only reads `Driver.CachedSessionContextUsage` (pure memory). An idle session's context
cannot change by construction — only a turn moves it — so the timer has nothing new to learn, and
the authoritative value is written back by turn completion (`pushContextUsage`) and attach. The
earlier version called `SessionContextUsage` per session per tick, one chain-signature stat batch
each, which showed up as a continuous process storm on remote hosts (and dial timeouts for an
unreachable one). Apply the same rule to any new background scan.

**And when a background task does touch the host, an error from it must not evict the shared
connection.** `shouldRedial` (`session/ssh_channels.go`) is the pool's single answer to "is this
connection broken?", because the remedy — drop and re-dial — tears down every other channel on it,
a live turn's stream included. Two errors look fatal and are not: a channel-open refusal (the
peer's `MaxSessions` ceiling: busy, not dead) and a **remote command's non-zero exit status** (the
channel opened, the command ran and chose to fail). Both bugs met in the deferred-purge retry: its
command exited 1 whenever the files were already gone — the *successful* outcome — so the item
never resolved, and every six-minute retry dropped the host's connection and killed whatever turn
was streaming on it. Route new pooled operations through `shouldRedial` rather than re-deriving the
condition, and make remote commands report shell-level success honestly (`exit 0`).

**The budget's ceiling is measured, not assumed.** `sshMaxChannels` (8) and `sshMaxStreamChannels`
(4) sit under the peer's real per-connection channel limit, and that limit is a *measurement*:
`SSHPool.ProbeChannelCeiling` opens channels on one pooled client until the peer refuses, exercised
by `TestLiveSSHChannelCeiling` (gated on `SPAWNER_SSH_LIVE=1`; the probe hogs a connection, so never
run it against a busy host). On the dev/loopback target — Arch OpenSSH with `MaxSessions` left at its
commented-out default — the measured ceiling is **10 concurrent channels**, which is what the 8/4
split is budgeted under. Re-run the probe before changing those constants or before assuming a new
host is as generous; the test fails if the ceiling drops below `sshMaxChannels`.

**A connection is not the unit of a host's concurrency — the pool holds several.** That measured
ceiling is *per TCP connection*, so while the pool kept one client per host the host's total
parallelism was 8 channels no matter how idle the machine was: a few turns holding stream slots and
every probe behind them queued. `poolEntry` therefore holds a **slice** of connections, each with
its own `channelBudget` (and `lastUsed`). `openChannel` hands a caller the **least-loaded** live
connection with a free slot; when every one is saturated it dials an **additional** connection for
that host, up to `sshMaxConnsPerHost` (4) — so the real ceiling is 4 × 8, and only at the connection
cap does anyone block. Per-connection dials still serialize under the entry lock (one cold host
can't stall another) and still honour the negative dial cache; a failed *extra* dial to a host that
still has a live link falls back to waiting rather than failing the caller. Crucially `drop` evicts
**one** connection, not the host's whole entry — a dead or refusing link can't punish the others,
which is why every operation returns the `*pooledConn` it used. The stream sub-budget stays
per-connection, so turns still can't starve probes on any given link.

Those extra connections are opened for a **burst** (a digest sweep, a prefetch storm), so they are
also **reaped**: `SSHPool.reapIdle` retires and closes every connection past the first that has
carried no channel for `sshIdleConnTTL` (2 min). It runs lazily on the next `acquireConn` and on
each keepalive tick, so a host that goes quiet converges back to its single warm connection with no
dedicated goroutine. The host's **first** connection is never reaped — it is the cached one the
keepalive owns, and dropping it would only make the next caller pay a dial. "Idle" is stricter than
"no channel open": a connection whose `channelBudget` has a slot taken (a caller about to open) also
counts as busy, and a caller that wins a slot on a connection retired in the same instant rechecks
`live()` and looks again — so a reap can never close the transport out from under work.

### Attach latency instrumentation (where a cold attach's wait actually goes)

Transcript prefetch, the digest cache and the display memo all exist to make tapping into a session
instant, and none of them could be judged by anything but feel. Both halves of the path now report
their own timing, so a change is verified by numbers.

**Server.** `session.StageTimer` (`internal/session/timing.go`) is a stage-timing collector carried
on the **context** rather than in call signatures — the stages live inside `Driver.DisplayHistory`
/ `DisplayDigest`, several layers below the caller that wants the numbers, and a nil timer (every
other caller: the digest sweep, the turn path) makes it a no-op. The driver attributes two stages —
`chain` (the SSH stat round trip that decides what's fresh) and `read` (the transcript fetch +
JSONL parse, which the display memo often serves outright) — plus a `digest=cached|computed` note.
`serveHistory` adds `assemble` (paging + scaffolding strip) and logs one line per request, mirroring
the digest sweep's:

```
history[web]: foreground unchanged, 0 row(s) in 41ms (queue 2ms, chain=39ms read=0s digest=cached)
history[api]: prefetch page, 30 row(s) in 812ms (queue 0s, chain=118ms read=677ms digest=computed assemble=3ms)
```

`queue` is time spent waiting for a lane (per-session coalescing, the concurrency gate) rather than
reading, so a stall caused by contention reads differently from a slow host. The lane label
(`foreground`/`prefetch`) keeps an attach's number unambiguous, and the outcome (`unchanged`,
`delta`, `page`, `page-back`) says which reply shape the client got — `unchanged` is the cache hit
an attach should be after.

**Client.** `net/AttachLatency` (commonMain, wired identically into both controllers) times focus →
first painted transcript and, crucially, labels its **source**: `prefetch-hit` (rows were already
held because a background prefetch fetched them this connection — `TranscriptPrefetcher.didPrefetch`),
`cache-hit` (held from an earlier visit) or `cold` (nothing held; the user waited on the network).
It reports two instants, because a warm attach paints at ~0 ms and is only *confirmed* later:

```
attach[<id>]: prefetch-hit/unchanged paint=0ms confirm=63ms rows=214
attach[<id>]: cold/page paint=740ms confirm=740ms rows=30
```

Judging a prefetch fix on paint alone would score every warm attach a success even when the server
then shipped a whole new page. Lines go to logcat (`Spawner` tag) on Android and the browser console
on web, through the one-line `platformLog` expect/actual seam — the client had no logger before this.

### Sandbox sessions (also without host root)

For `sandbox`-target sessions the container's lifetime is **bound to the session**, not the turn:
the `SandboxExecutor` creates a long-lived container at spawn (`Ensure` → `run -d … sleep
infinity`, named `spawner-sbx-<hex>` from `Session.Container`), each turn runs via `exec -w <dir>`
into it, and it's destroyed when the session is deleted (`Remove` → `rm -f`). So packages
installed and services started in one turn persist to the next — a real environment, not a fresh
box per turn. `Ensure` is idempotent and re-run before every turn, so a container lost to a server
restart or manual `rm` is transparently recreated. Spawn-time `Ensure` is best-effort (logged, not
fatal); a hard runtime failure surfaces on the first turn. Use a **rootless Podman / rootless
Docker** runtime (`SPAWNER_SANDBOX_RUNTIME`) so none of this needs host root — the sandbox gets
root *inside itself* and a disposable FS. Session `Dir` is bind-mounted same-path (so the
transcript's project encoding matches the host); the host user's `$HOME` is also bind-mounted into
the **sandbox container** **read-write at the same path** by default (`SandboxExecutor.HomeMount`,
set from `$HOME`), so dotfiles, `~/.claude`, `~/.codex`, and project checkouts are writable inside
the sandbox exactly as on the host.
Add anything outside `$HOME` via `SPAWNER_SANDBOX_MOUNTS`. Lifecycle hooks live in the gateway spawn (`ensureSandbox`) and
delete (`removeSandbox`) paths; `Driver.EnsureContainer`/`RemoveContainer` bridge to the executor.
At startup `Driver.ReconcileContainers` sweeps **orphans** — managed containers (matched by the
`spawner-sbx-` name prefix) whose session record no longer exists, e.g. deleted while the server was
down — so they don't accumulate; live sessions' containers are left for `Ensure`-before-turn. The
server drives the runtime (create/exec/remove/list) directly as the user.

**Sandbox on a containerized (SSH-native) server.** A containerized server has no container
runtime of its own, so the `SandboxExecutor` is wired with the same
`SSHPool` and drives **rootless podman on the host over SSH** — every `run`/`exec`/`inspect`/`rm`
runs on `localhost` (the co-located host, over loopback SSH), exactly the way host turns already
do. The exec turn streams over SSH via the shared `SSHPool.Stream`/`streamRemote` helper (the same
cancelable, process-group-killed path as a host turn); lifecycle control goes over `SSHPool.Run`.
Every mount/dir path is a **host** path (session `Dir` and `SPAWNER_SANDBOX_MOUNTS` already are,
since sessions are created against the host filesystem), and `HomeMount` (`-v $HOME:$HOME`, run by
podman **on the host**) makes the sandbox write its transcript into the host user's `~/.claude`.
The server then reads that transcript — and runs discovery — **over SSH on `localhost`**, not off
its own filesystem: a `sandbox` session carries no `Session.Host`, and `claudeFSFor("")` maps that
empty host to the loopback host and returns the SSH-backed `claudeFS`. Nothing about the sandbox
touches the server container's own `/data` or `$HOME`, which is why those bind mounts are gone from
`docker-compose.yml` (only `state` and the whisper models dir remain).

The `SandboxExecutor`'s local-child-process path (`Pool` nil) survives only for unit tests; the
running server always wires the pool. This is what lets the `sandbox` target — e.g. a
`target: sandbox` session with no `Session.Host` — run on the containerized server, which
otherwise fell back to the host executor and failed with "no host set".

### Net security posture

No component holds host root: the server is a plain user process and sandboxes use a rootless
runtime. Cost is just the `Executor` seam. See `docs/protocol.md` if a spawn-time target selector
reaches the wire protocol (it may not — the dialog can carry it server-side, like `rename`).

## Detached background jobs (survive the turn boundary)

A turn is one short-lived headless `claude` process, so a command that must outlive it can't ride
Claude's in-process `run_in_background`. The `spawner-job` wrapper (embedded via `go:embed` in
`internal/session/bgjob`, staged to each target on demand) launches the command with its **own**
`setsid`/`nohup`, stdin `/dev/null`, and output to a log — so neither the SSH `kill -pgid` teardown
nor the host executor's group-SIGKILL can reach it. Jobs are recorded in an on-target registry
**keyed by working dir** (stable across `clear`/`compress` session-id rotation), the source of
truth; `Session.Jobs`/`PendingNotes` are the persisted mirror.

`Driver.RunOnTarget` runs short commands on the session's *same* target (host fork / `SSHPool.Run` /
`podman exec`), which the gateway's `reconcileJobs` uses at each turn boundary and on attach to poll
the registry. A newly-finished job's bounded output becomes a framed `PendingNotes` entry that
`dictate` prepends to the next turn's prompt (so Claude is told), and `JobsPrimed` gates a one-per-
context instruction telling Claude to use the wrapper. Reconcile/stage errors are swallowed so they
never block a turn. Caveat: sandbox jobs live only as long as the container.

**Per-session attribution.** Because the registry is dir-keyed, two sessions in the *same directory*
(e.g. a host and a sandbox session, which share the bind-mounted home) see each other's jobs. To keep
one session from adopting and announcing a job another started, each job is **stamped with the
launching `session_id`**: the server bakes `--owner <id>` into the staged hook command, the hook
threads it into the rewritten `start`, and `list --json` reports it. `reconcileJobs` then **skips any
record whose owner this session doesn't own** (`Session.OwnsID` matches the current id, `PriorIDs`,
and `History` ids, since the id rotates on `clear`/`compress`/backend-switch) — leaving it intact for
its real owner. A job with **no** owner (started before stamping existed) falls back to the old
dir-attributed behaviour.

Enforcement (not just priming): the turn injects a Claude **PreToolUse hook** via `--settings`
(`HookSettingsJSON` → `TurnSpec.SettingsJSON` → the Claude agent's argv) whose `Bash` matcher runs
`spawner-job hook --owner <session_id>`. On a `run_in_background` launch that subcommand emits a
PreToolUse `updatedInput` that **transparently rewrites** the call to
`spawner-job start --owner <id> '<original cmd>'` (jq `@sh` quotes the command; `run_in_background` is
cleared; the owner stamps the job for per-session attribution above) — no cancellation, the same Bash
tool just runs the wrapped command. Fallbacks preserve enforcement: no jq → exit 2 to block with a redirect; unstaged wrapper →
the hook is a graceful no-op. Hooks fire under `--dangerously-skip-permissions`, so it's a hard gate.

## Transcription (internal/transcribe)

The gateway depends only on the `Transcriber` interface; there are **two implementations** and
either can back it:

- **`RemoteWhisper`** (`remote.go`) — POSTs the WAV to a **resident whisper HTTP server**
  (`/inference`). This is the preferred path on this host, which has an **Nvidia GPU**: a
  CUDA-built **WhisperX** server keeps the model warm (`medium.en`, `:8572`) — same `/inference`
  contract as whisper.cpp but with accurate, stable word timestamps — handling both real
  dictation and the live hands-free draft + end-token detection. An optional second, fast draft
  server (a whisper.cpp server on `:8571`) can offload the cheap high-frequency work so it never
  blocks the accurate model. The whisper (and Kokoro TTS) containers no longer live in this repo —
  they were moved out to the standalone **`/data/speech_services`** stack (which carries both the
  WhisperX and whisper.cpp servers), so nothing here changes but where the containers are launched.
  Enabled via `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL`.
- **`WhisperCPP`** (`transcribe.go`) — shells out to the **whisper.cpp CLI** (one process per
  utterance), `exec`'d like `claude`/`tmux`, no server. The fallback when no whisper URL is set.
  It size-picks a model per clip (tiny/base/small) from `SPAWNER_WHISPER_MODEL{,_FAST,_BASE}`.

Opus clips are decoded to 16 kHz mono WAV with **ffmpeg** first (whisper can't read Opus). STT is
disabled unless a model/URL is configured; when disabled the audio path returns `not_implemented`
but text utterances still work. Swapping to faster-whisper or a cloud API (e.g. Groq
large-v3-turbo) stays a one-file change behind the `Transcriber` interface.

Whisper hallucinates on silence (it fills quiet stretches with looped YouTube-outro phrases), so
the resident server images run with **Silero VAD + non-speech-token suppression** as entrypoint
defaults — those defaults live with the whisper image in the `/data/speech_services` stack.

Known limitation: STT output is all-lowercase, so sessions can't be created in directories with
uppercase letters by voice. Acceptable; documented in `docs/commands.md`.

## Repository layout

```
/server                         Go server (module: github.com/bam/claude_spawner/server)
  main.go                       entrypoint: HTTP server, graceful shutdown, /healthz, /ws
  internal/gateway/             WebSocket gateway: auth, dispatch, dialog FSM, dictation loop
    gateway.go                  Server, conn, auth handshake, read loop, message dispatch
    ops.go                      control commands (list/attach/detach/kill/status) + dictate
    dialog.go                   spawn dialog FSM, session creation, name sanitizing
    audio.go                    audio path: wake/binary/audio_end -> WAV -> STT -> utterance
    stream.go                   hands-free streaming: live pending draft, end-token commit
    jobs.go                     running-turn tracking: activity/files breadcrumbs, diff summary
    inflight.go                 per-session in-flight turn registry (abort, restart interrupts)
    ask.go                      interactive-mode clarifying-question (ask) extraction
    browse.go                   host-scoped directory listing for the New-session picker (listing);
                                  lists the chosen host's FS over SSH from "/" (not the local roots)
    messages.go                 wire message constructors
    *_test.go                   httptest+ws integration (auth, spawn, dictation, ask, stream)
  internal/agent/               AI backend registry: Agent type + Registry (agent.go), shared turn vocabulary (turn.go), one self-contained file per backend family (claude.go, codex.go, opencode.go)
  internal/session/session.go   headless driver: Driver.Turn (per-agent args + parser), parseStream/parseCodexStream
  internal/session/executor.go  pluggable Executor: HostExecutor (direct exec) + SandboxExecutor (runtime)
  internal/session/store.go     durable session registry (file-backed, atomic writes); Session.Target/Container
  internal/session/settings.go  server-global preferences persisted to settings.json (survives restart; e.g. resident whisper model)
  internal/session/discover.go  scan ~/.claude/projects for all Claude sessions (adopt/discover)
  internal/session/transcript.go read/stitch Claude on-disk transcripts for `history` (spans clears)
  internal/session/codex_transcript.go  codexFS: read Codex rollout files for `history`/context badge
  internal/session/opencode_transcript.go  opencodeFS: `opencode export`/`session delete` for `history`/context badge
  internal/command/command.go   utterance -> intent parser + StripWake
  internal/command/registry.go  Command registry (single source of truth) + RegistryJSON
  internal/transcribe/          Transcriber interface: WhisperCPP (CLI) + RemoteWhisper (HTTP)
  internal/projects/projects.go spoken-path term tokenizing + fuzzy ranking (Terms/Rank) for the resolver
  internal/tmux/tmux.go         detect a live interactive `claude` in a pane (ClaudeDirs)
  internal/usage/               per-turn token cost tracking + Estimator (server-global usage %)
  internal/config/config.go     env config + spawn-path validation
  internal/docsync/             drift tests: env vars/wire messages/error codes ↔ docs (config/protocol)
  cmd/wsclient/main.go          text client for manual testing; -audio streams a WAV
  cmd/gencommands/main.go       regenerate docs/commands.json from the command registry
  main.go                       server entrypoint (built into the Docker image from server/Dockerfile)
docker-compose.yml              spawner-server gateway + wakeword detector (STT/TTS containers live in /data/speech_services)
/sandbox                        Arch-based sandbox image (Containerfile) for `target: sandbox` sessions (see sandbox/README.md)
/deploy                         containerized server compose + env example + container-rebuild + claude-log helpers (see deploy/README.md)
/android                        Android app (Kotlin/Compose) — see android/README.md
/docs
  protocol.md                   WebSocket message schema (single source of truth)
  commands.md                   "hey buddy" command grammar + dialog flows
  commands.json                 command list generated from the registry (consumed by the app build)
README.md / CLAUDE.md / TODO.toml / .gitignore
```

Architectural status: the **full voice loop works end-to-end and is verified live** against
`claude` 2.1.196 — spawn dialog → mkdir → attach → dictation turn → real reply → `--resume` recall
across reconnects. Real **audio** turns are verified too: a spoken/`jfk.wav` clip → Whisper →
`transcript` → `utterance` → Claude reply, on both the resident GPU whisper server and the CLI
fallback (the shell-out contract is also unit-tested with a fake binary). The **Android app** is
built and verified live on the emulator and the Pixel 8a. (Task-level status — what's built vs.
next — lives in `TODO.toml`, not here.)

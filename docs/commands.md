# Command grammar

The authoritative "hey buddy" command vocabulary and dialog flows. Both the Android app and the
Go server reference this. When you change a command, change it here first.

## Conventions

- Every control command is prefixed with the wake word **"hey buddy"** or **"hey bud"**. The wake
  token has an **alias list** (`command.wakePhrases`, the single source of truth — the wake-word
  analogue of a command's aliases), so common whisper mishearings also fire it: two-word forms
  "hey body" / "hey buddie" / "hey budy", and **one-word collapses** where whisper runs the phrase
  together — notably **"everybody"** (and "heybuddy"). Add a new mishearing by extending that list.
  (One-word aliases are ordinary English words, so they wake more eagerly — e.g. "everybody knows"
  strips a wake; the set is kept small on purpose.)
- **Custom wake words (per client):** the app's Commands settings can set extra wake word(s) (the
  `wake_token` field of the `hello` handshake). They're accepted **alongside** the built-in "hey buddy"
  family, not instead of it — the server folds them in via `command.WakePhrase` → `StripWakeWith` /
  `SplitWakeWith`, and biases whisper toward them (`vocabBias`) so a non-"hey buddy" word transcribes
  reliably. The field is **comma-separated**, so you can configure several variants ("hey buddy, hey
  bud, ok buddy") — handy because whisper mis-hears the wake phrase in noise; any variant fires.
  Blank = built-in only. A custom word has no curated mishearing aliases, so pick ones whisper hears
  cleanly.
- **Dictation gate ("speak token"):** for hands-free use in noisy rooms, Commands settings has a
  **dictation gate** — a `speak_token` (comma-separated start marker, e.g. "take a note") plus a
  `dictation_gate` switch. When the gate is on, un-command speech is dictated **only** when it
  follows the speak token, up to the end token; everything else (background chatter, radio, other
  people) is discarded instead of forwarded to the session. Commands ("hey buddy …") are **never**
  gated, so barge-in still works. Server-side the speak token matches via `command.SplitOn` (its own
  phrase set, independent of the wake word) and is biased into `vocabBias`. Blank speak token, or the
  switch off, keeps the ungated behavior (all non-command speech dictates). See `docs/protocol.md`.
- The wake word is detected **server-side, in the transcript** (`command.StripWake`) — there is no
  on-device wake engine. The app streams speech (VAD-gated) to the server, which transcribes it and
  applies this grammar.
- Command matching is **precise**, not loose substring: a command is a short utterance led by a
  command verb ("list", "detach", "attach to X", "kill X", "spawn … session", "status") or a
  distinctive phrase. Longer utterances ("list the files in this module", "spawn a goroutine") are
  treated as dictation, not commands — so plain speech to Claude isn't hijacked.
- While **attached**, a transcript that isn't a command is **dictation**, forwarded to the session.
  A wake-prefixed non-command ("hey buddy, refactor this function") is also dictated (wake-stripped).
- **Hands-free** (VAD-gated clips): an utterance with no wake word is dictated only inside the ~8 s
  **follow-up window** after a reply (natural back-and-forth); outside it, no-wake speech is dropped
  as background chatter. `Stop` ("hey bud stop") always interrupts speech, in any context.
- **Chained commands in one utterance.** Because clips accumulate until the hands-free end token
  (or silence commit), a single committed message can contain several wake phrases. The server
  splits on **every** "hey buddy" and runs the commands **in order** ("hey buddy list, hey buddy
  detach" → list, then detach); any leading text before the first wake is the dictation. Everything
  runs in **spoken order**: the leading dictation was spoken first, so it's sent to the attached
  session **before** the commands run — so "<dictation> hey buddy detach" lands the dictation in the
  session before the detach removes it (the trade-off: "<dictation> hey buddy attach" dictates into
  the *old* session, not the newly attached one). `cancel`
  is a **reset point**: it scraps everything **before** it — the leading dictation and any earlier
  commands — while commands **after** it still run ("hey buddy list, hey buddy cancel, hey buddy
  detach" → only detach). The **last** cancel wins, and a trailing cancel with nothing after it
  scraps the whole committed message — handy when the end token misfired and you kept talking.
  (`SplitWake` keeps only the last command; `SplitWakeAll` is what the commit path uses.)
- **Attach-then-dictate is two utterances, by design.** You **cannot** attach and dictate into the
  *newly* attached session in one breath: leading dictation goes to whatever is attached *at that
  moment* (the old session, per spoken order), and words after "hey buddy attach …" are consumed by
  the command. To speak into a session you just attached, use a **second utterance** after the
  attach lands. This is a deliberate simplicity choice — the wake word stays the single divider
  between server commands and session dictation; don't re-litigate interleaving them.

## Spoken-path conversions

The server normalizes dictated path fragments into real paths:

| Spoken                                  | Result                |
|-----------------------------------------|-----------------------|
| "data claude underscore claude"         | `/data/claude_claude` |
| "slash data slash projects"             | `/data/projects`      |
| "underscore"                            | `_`                   |
| "dash" / "hyphen"                       | `-`                   |
| "dot"                                   | `.`                   |

A spawn location is a **full absolute path**, resolved segment-by-segment against the target host's
real filesystem (over SSH) with per-segment fuzzy matching — there is no spawn-root jail, so a
session may spawn anywhere on the target.

## Control commands

| Intent          | Canonical phrasing                       | Effect                                            |
|-----------------|------------------------------------------|---------------------------------------------------|
| `spawn`         | "spawn a new session" / "new session called bugfix in data" / "new session in git personal on codex with sandbox profile" / "spawn a new project in git personal" | Starts a session. Everything after the noun is optional; anything you don't say uses a default. **Name** — "called <name>" / "named <name>" (default: the folder basename). **Location** — after "in"/"at"/"under" (default: your **home directory**). **Provider** — "on codex" / "on opencode" / "a codex session" (default: Claude); the session is stamped with that backend and its default model. **Profile** — "with <name> profile" / "profile <name>" (default: the marked-default execution profile). When the location resolves to a concrete folder (or is omitted → home), the server **skips the dialog and creates + attaches immediately** with those defaults. It falls back to the interactive dialog (below) for "new project" (folder creation), a location it can't resolve or only fuzzily matches, or one that lands on a root/namespace with sub-folders to choose among. |
| `attach`        | "attach to `<name>`"                     | Attaches; subsequent speech is dictated           |
| `detach`        | "detach" / "stop dictating"              | Leaves passthrough mode                            |
| `swap`          | "swap" / "swap back" / "previous session" / "last session" | Jumps back to the session you were attached to **just before** this one — a two-way toggle, so saying it again returns you. The server remembers the previous session per connection (set whenever you attach elsewhere, and on detach), so no name is needed. The same jump is bound to a **right-to-left swipe** on the chat screen in the app. Speaks "no previous session…" if you haven't been anywhere else yet, or "the previous session is gone" if it was killed meanwhile. |
| `list`          | "list sessions" / "what sessions"        | Reads back known sessions                          |
| `kill`          | "kill session `<name>`"                  | Confirms, then deletes the session's registry record |
| `status`        | "what's the status" / "what's it doing"  | Snapshot of the attached session's recent output  |
| `read_last`     | "read last" / "read last 3" / "read the last two" / "replay last" / "replay that" / "say that again" / "repeat that" | Re-reads aloud (TTS) + scrolls to the last N Claude replies in the current session (N defaults to 1; digit or number-word). |
| `clear`         | "clear" / "clear context" / "clear session" / "clear the context" / "reset context" / "start fresh" / "wipe context" | Rotates the attached session's Claude context to a fresh `session_id`, so the next dictation replays **no** prior history (no re-read, no re-billing of the whole transcript). The old transcript is **kept on disk** and still shows in `history`. Deliberately NOT matched: "clear history" — clear never deletes. No-op if no turn has run yet; refused while a turn is in flight. |
| `compress`      | "compress" / "compress context" / "compact" / "compact context" / "condense context" / "summarize the context" / "compact it" | The `/compact` counterpart to `clear`: asks Claude to **summarize** the conversation, then rotates to a fresh `session_id` and carries that summary forward as a seed prepended to the **next** dictation — so Claude continues with the context **condensed** instead of dropped. Costs one model turn (the summary). The old transcript is **kept on disk** and still shows in `history`. No-op if no turn has run yet; refused while a turn is in flight. A following `clear` discards the pending summary. |
| `cancel`        | "cancel" / "cancel that" / "never mind"  | Aborts the current dialog; in a chained utterance, scraps everything **before** it (dictation + earlier commands) while later commands still run — the last cancel wins |
| `stop`          | "hey bud stop" / "stop talking" / "quiet"| Barge-in: stops TTS everywhere; never dictated. Also, pressing push-to-talk stops speech client-side. |
| `abort_turn`    | "stop the turn" / "cancel the turn" / "abort" / "stop working" | Cancels the running Claude turn (kills the child). Distinct from `stop` (TTS) and `cancel` (discard a composing message). |
| `rename`        | "rename to `<name>`" / "rename this session `<name>`" / "call this `<name>`" | Renames the session you're **attached to** (no explicit old name — it always targets the current session). The spoken name is sanitized to one token (multi-word is joined/hyphenated), so "rename to my backend" → `my-backend`. Refused if you're not attached, if no name is given, or if the name is already taken; speaks a confirmation on success. Distinct from the app's rename UI, which renames any session by explicit old→new. |
| `usage`         | "usage" / "how much usage left" / "check usage" / "how much have I used" | Reports the Claude plan's usage — session and weekly **% used** with reset times — by running `claude -p "/usage"` (the same numbers the desktop TUI's `/usage` shows). The app opens a usage sheet (percent-used bars + the local contributing breakdown); the voice form also speaks a one-line summary. Also reachable via the 📊 Check usage button in the sessions drawer. On-demand — a real, lightweight claude invocation, not per-turn. |
| `list_models`   | "list models" / "what models" / "which models" | Speaks the models the **attached session's AI backend** offers, numbered in catalogue order, marking the current one. The number is what `use_model` takes — ordinal selection sidesteps hard-to-say model names (e.g. Codex's `gpt-5.5` reasoning presets). Refused if not attached. |
| `use_model`     | "use model `<number>`" / "switch to model `<number>`" / "select model `<number>`" | Switches the attached session's model to the N-th from `list_models` (1-based; digit or number-word — "use model three"). Durable on the session; takes effect on the **next** message (a turn already running finishes on the old model). Refused if not attached, or if the number is out of range. |
| `help`          | "help" / "what can you do" / "commands" | Speaks the list of available commands (generated from the command registry). |
| `scratch`       | "scratch on" / "scratch off" / "scratch mode on" / "scratch" | Toggles **scratch mode**, a transcription-quality test: while **detached** (no session to dictate to), every recognized utterance that isn't itself a command is read straight back to you via TTS, so you hear exactly what Whisper heard. `on`/`off` set it explicitly; a bare "scratch" flips it. Has no effect while attached (dictation still flows to the session) — detach first. Commands still work in scratch mode (the whole detached utterance is parsed as a command first), so speak plain sentences to test STT. |
| `summary_only`  | "summary only" / "summaries only" / "speak everything" | Toggles **summary-only speech** for long, multi-step turns. When on, the client reads aloud **only the final result** of a turn; each intermediate streamed step (the step-by-step narration, subagent summaries) plays a soft, warm beep instead of being spoken — so you know work is happening without hearing every detail. Everything is still shown on screen. "summary only" (or "summaries only" / "summary mode" / "just the summary") turns it on; "speak everything" (or "say everything" / "read everything") turns it off; "summary only off" also turns it off. The state lives on the client — persisted, and mirrored by the **Summary only** switch on the Audio settings page — so the server holds none. Sends a `speech_mode` message (see `docs/protocol.md`). |
| `list_jobs`     | "list jobs" / "background jobs" / "what jobs" | Speaks the attached session's **detached background jobs** (the `spawner-job` jobs that survive turns — see `README.md`), numbered, each marked running or finished. The number is what `kill_job` takes. Refused if not attached. |
| `kill_job`      | "kill job `<number>`" / "stop job `<number>`" / "cancel job `<number>`" | Terminates the N-th background job from `list_jobs` (1-based; digit or number-word), taking its whole process group down. A **number is required**, so it can't be confused with `kill` (delete a session) or `abort` (cancel the running turn). Refused if not attached, or if the number is out of range. |
| `job_status`    | "job status" / "how are the jobs" | Speaks a one-line summary — how many background jobs are running vs finished — the quick check versus the full `list_jobs` listing. Refused if not attached. |
| `restart`       | "restart the server" / "rebuild the server" / "restart spawner" | Rebuilds and restarts the **spawner server itself** by firing `SPAWNER_RESTART_CMD` (the same action as the app's restart button) — see `CLAUDE.md`'s config section for what that command does per deployment. Requires the noun "server"/"spawner" so it can't be confused with restarting a session or aborting a turn. Refused if restart isn't configured. **This drops the current turn** (including your own) while the server bounces. |

### Non-voice: the command tray

Every argument-free control command can also be fired by hand — no voice needed. **Swipe up on the
message box** to reveal a **command tray** above it: one tap button per command that takes no extra
argument (`abort`, `cancel`, `clear`, `compress`, `detach`, `help`, `list`, `list models`, `read last`,
`status`, `stop`, `usage`). Tapping a button sends the command (as a wake-prefixed utterance, so the server
treats it as a control command even while attached) and closes the tray; swipe back down to dismiss
it without firing. The buttons are derived from the generated `COMMANDS` list — any command whose
aliases contain a `<placeholder>` (`attach`, `kill`, `spawn`) is excluded, since a button can't
supply the argument. It never drifts from this grammar. Dismiss the tray without firing by
swiping back down, tapping anywhere outside it (the chat, the bars), or tapping the message box to
start typing.

## Dialog: spawn a new session

**Fast path first.** For a plain "new session" (not "new project"), the server tries a one-shot
spawn before falling back to the dialog: if the spoken location is a full path that resolves cleanly
against the target host's filesystem, it creates the session and attaches right away, applying the
default provider and profile for whatever wasn't named ("hey buddy, new session in slash home slash
bam slash git on opencode"). A **bare** "new session" (no path), or one whose path is ambiguous or
can't be found, drops into the dialog, which asks for — or reconfirms — the full path.

A small state machine. The server drives the prompts; the app speaks them (TTS) and streams the
user's replies back. The user speaks a **full absolute path**, which the server resolves
segment-by-segment against the real filesystem (see below).

```
[idle]
  user: "hey buddy, spawn a new session"
  app : "where to? say the full path, like slash home slash bam slash git."
[await_path]
  user: "slash home slash bam slash git"  -> resolve each segment against the real FS
    -> resolves cleanly       -> [await_target]  (or [await_attach] when no sandbox)
    -> ambiguous / not found  -> reprompt, stay [await_path]
    (new-project mode: the last segment names a folder to create under the resolved parent)
[await_target]   (only when SPAWNER_SANDBOX_IMAGE is configured; otherwise skipped)
  app : "run <dir> on the host, or in a sandbox?"
  user: "host"                           -> Session.Target = host    -> [await_attach]
  user: "sandbox"/"container"/"isolated" -> Session.Target = sandbox -> [await_attach]
[await_attach]
  app : "found <name>. want to attach?"
  user: "yes"  -> persist session + attach -> [attached]
  user: "no"   -> persist session (ready to attach later), stay [idle]
  user: "cancel" -> abort, nothing persisted   (cancel works from any state)
```

**Path resolution.** A separator is either a literal "/" or the spoken word "slash"; each segment's
remaining words are fuzzy-matched against the *real* immediate subdirectories at that level (listed
over SSH on the target host — the machine the session will run on, not the server's container). A
segment that matches exactly one child auto-corrects to it with no confirmation ("colmb" -> `home`
when `home` is the closest real child of `/`); a segment matching several children with no clear
winner, or none at all, reprompts for the whole path. Per-segment matching drops filler words (the,
a, repo, folder, …) and is separator- and camelCase-aware.

**Transcription-error tolerance.** Segment matching uses edit distance, so whisper slips like "get"
-> `git` or "colmb" -> `home` still resolve — the candidates are constrained to the directories that
actually exist at each level, which is what makes the correction reliable. (`projects.Levenshtein` /
`FuzzyEqual`, used in `Rank`.)

**Entry points.** The whole path can ride on the command:
- `spawn a session in slash home slash bam slash git` -> resolve and spawn there (skip the "where?" prompt).
- `spawn a new project in slash home slash bam slash newthing` -> resolve the parent, create the
  final folder, then go straight to the attach question.

Edge cases:
- Path ambiguous or not found -> "i couldn't place that path — say the full path again."
- Spawn fails (claude not found, mkdir error) -> spoken error feedback.

## Dictation (attached mode)

```
[attached]
  user: "<anything not matching a control command>"
  -> server: Driver.Turn(session, text)  ==  claude -p "<text>" --resume <session_id>
             --output-format stream-json  (headless; NOT tmux send-keys)
  -> tool_use events  -> optional spoken breadcrumbs ("running a command…")
  -> result event     -> clean text -> app displays + reads aloud
  user: "hey buddy, detach"  -> [idle]
```

Reserved-word collision: if the user genuinely needs to dictate a phrase that looks like a
command, require the wake word for all control commands so plain dictation is never intercepted.

## Adding or changing a command

The **command set** has a single code source of truth: `server/internal/command.Registry` (a list
of `Command{Kind, Title, Aliases, Description, Example}` structs). It flows to the Android app
through a real, checked pipeline — **no command list is ever hand-maintained in the app**:

```
command.Registry (Go)  →[ go run ./cmd/gencommands ]→  docs/commands.json
                       →[ Gradle generateCommands, wired into preBuild ]→
                       app/build/generated/commands/…/Commands.kt (the COMMANDS list)
                       →  consumed by MainActivity.kt's Commands screen + alias editor
```

The whole procedure for adding or changing a command:

1. Edit `command.Registry` (and `Parse` in `command.go`). Tests enforce the two ends stay honest:
   every `Example` must `Parse` to its `Kind`, and every user-facing `Kind` must be registered.
2. `go run ./cmd/gencommands` to rewrite `docs/commands.json` (a drift test fails if it's stale).
3. Rebuild the APK. `generateCommands` runs before every build and regenerates `Commands.kt` from
   the JSON, so the new/changed command appears in the app automatically. **You never touch Kotlin**
   — `Commands.kt` is a build artifact (under `app/build/`, git-ignored), not a source file.

The app therefore can't drift from the server grammar or ship an undocumented command; the only way
a registry change reaches an installed app is the APK rebuild in step 3.

The generator itself is drift-tested too: `:app:testDebugUnitTest` runs `CommandsSyncTest`, which
compares the compiled-in `COMMANDS` list against `docs/commands.json` entry by entry — a generator
bug that drops or mangles a command (escaping, sorting, a schema change it ignores) fails the test.

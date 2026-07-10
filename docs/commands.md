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
- **Custom wake token (per client):** the app's Commands settings can set an extra wake word (the
  `wake_token` field of the `hello` handshake). It's accepted **alongside** the built-in "hey buddy"
  family, not instead of it — the server folds it in via `command.WakePhrase` → `StripWakeWith` /
  `SplitWakeWith`, and biases whisper toward it (`vocabBias`) so a non-"hey buddy" word transcribes
  reliably. Blank = built-in only. A custom word has no curated mishearing aliases, so pick one
  whisper hears cleanly.
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

## Spoken-path conversions

The server normalizes dictated path fragments into real paths:

| Spoken                                  | Result                |
|-----------------------------------------|-----------------------|
| "data claude underscore claude"         | `/data/claude_claude` |
| "slash data slash projects"             | `/data/projects`      |
| "underscore"                            | `_`                   |
| "dash" / "hyphen"                       | `-`                   |
| "dot"                                   | `.`                   |

A leading bare word (no "slash") is interpreted relative to the configured **spawn root**.
Reject paths that escape the allowed root unless the user explicitly opts in.

## Control commands

| Intent          | Canonical phrasing                       | Effect                                            |
|-----------------|------------------------------------------|---------------------------------------------------|
| `spawn`         | "spawn a new session" / "spawn a session in git personal" / "spawn a new project in git personal" | Starts the spawn dialog (see below). An inline location after "in"/"at" jumps straight there; "new project" switches to create mode. An inline **backend** — "spawn a **codex** session" or "spawn a session **on codex**" — picks the AI for the new session (default is Claude); the session is stamped with that backend and its default model. When a sandbox image is configured (`SPAWNER_SANDBOX_IMAGE`), the dialog also asks whether to run the session on the **host** or in a **sandbox**. |
| `attach`        | "attach to `<name>`"                     | Attaches; subsequent speech is dictated           |
| `detach`        | "detach" / "stop dictating"              | Leaves passthrough mode                            |
| `list`          | "list sessions" / "what sessions"        | Reads back known sessions                          |
| `kill`          | "kill session `<name>`"                  | Confirms, then deletes the session's registry record |
| `status`        | "what's the status" / "what's it doing"  | Snapshot of the attached session's recent output  |
| `read_last`     | "read last" / "read last 3" / "read the last two" / "say that again" / "repeat that" | Re-reads aloud (TTS) + scrolls to the last N Claude replies in the current session (N defaults to 1; digit or number-word). |
| `clear`         | "clear" / "clear context" / "clear session" / "clear the context" / "reset context" / "start fresh" / "wipe context" | Rotates the attached session's Claude context to a fresh `session_id`, so the next dictation replays **no** prior history (no re-read, no re-billing of the whole transcript). The old transcript is **kept on disk** and still shows in `history`. Deliberately NOT matched: "clear history" — clear never deletes. No-op if no turn has run yet; refused while a turn is in flight. |
| `compress`      | "compress" / "compress context" / "compact" / "compact context" / "condense context" / "summarize the context" / "compact it" | The `/compact` counterpart to `clear`: asks Claude to **summarize** the conversation, then rotates to a fresh `session_id` and carries that summary forward as a seed prepended to the **next** dictation — so Claude continues with the context **condensed** instead of dropped. Costs one model turn (the summary). The old transcript is **kept on disk** and still shows in `history`. No-op if no turn has run yet; refused while a turn is in flight. A following `clear` discards the pending summary. |
| `cancel`        | "cancel" / "never mind"                  | Aborts the current dialog                          |
| `stop`          | "hey bud stop" / "stop talking" / "quiet"| Barge-in: stops TTS everywhere; never dictated. Also, pressing push-to-talk stops speech client-side. |
| `abort_turn`    | "stop the turn" / "cancel the turn" / "abort" / "stop working" | Cancels the running Claude turn (kills the child). Distinct from `stop` (TTS) and `cancel` (discard a composing message). |
| `rename`        | "rename to `<name>`" / "rename this session `<name>`" / "call this `<name>`" | Renames the session you're **attached to** (no explicit old name — it always targets the current session). The spoken name is sanitized to one token (multi-word is joined/hyphenated), so "rename to my backend" → `my-backend`. Refused if you're not attached, if no name is given, or if the name is already taken; speaks a confirmation on success. Distinct from the app's rename UI, which renames any session by explicit old→new. |
| `usage`         | "usage" / "how much usage left" / "check usage" / "how much have I used" | Reports the Claude plan's usage — session and weekly **% used** with reset times — by running `claude -p "/usage"` (the same numbers the desktop TUI's `/usage` shows). The app opens a usage sheet (percent-used bars + the local contributing breakdown); the voice form also speaks a one-line summary. Also reachable via the 📊 Check usage button in the sessions drawer. On-demand — a real, lightweight claude invocation, not per-turn. |
| `list_models`   | "list models" / "what models" / "which models" | Speaks the models the **attached session's AI backend** offers, numbered in catalogue order, marking the current one. The number is what `use_model` takes — ordinal selection sidesteps hard-to-say model names (e.g. Codex's `gpt-5.5` reasoning presets). Refused if not attached. |
| `use_model`     | "use model `<number>`" / "switch to model `<number>`" / "select model `<number>`" | Switches the attached session's model to the N-th from `list_models` (1-based; digit or number-word — "use model three"). Durable on the session; takes effect on the **next** message (a turn already running finishes on the old model). Refused if not attached, or if the number is out of range. |
| `help`          | "help" / "what can you do" / "commands" | Speaks the list of available commands (generated from the command registry). |

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

A small state machine. The server drives the prompts; the app speaks them (TTS) and streams the
user's replies back. Navigation is **hierarchical**: pick a root (the basename of each configured
root, e.g. `git` / `data`), then walk down the directory tree. You can also say a whole path at
once ("git personal askii").

```
[idle]
  user: "hey buddy, spawn a new session"
  app : "where to, bud? say git or data — then a folder, like 'git personal'."
[await_root]
  user: "git"                 -> go to that root
  user: "git personal askii"  -> descend all matching segments in one go
    -> land on a directory, then browseInto() decides:
[await_child]   (when the landing dir is a root or a namespace of repos)
  app : "which folder in <dir>? <a>, <b>, ..."   (<=8 kids) 
        or "<dir> has a lot of folders — say a name, or 'list' to hear some."
  user: "<subfolder>"   -> descend (best fuzzy match among children)
  user: "personal"      -> namespace -> stays [await_child] one level deeper
  user: "list" / "list all" / "list recent"  -> read child names (short sample /
        everything alphabetical / everything newest-first by mtime), stays [await_child]
  user: "here"/"use it" -> use the current folder -> [await_attach]
  user: "<new name>"    -> nothing matches -> [await_create]
[await_confirm]   (only when a leaf was reached by a *stretched* fuzzy match —
                   the folder carries a token you never said, e.g. "mail" -> "mail_play")
  app : "i don't see that exactly — did you mean <name>? yes or no."
  user: "yes"  -> proceed with that folder -> [await_target]
  user: "no"   -> back up to its parent -> [await_child]
[await_create]
  user: "yes"  -> mkdir <browse>/<name> (validated against roots) -> [await_target]
  user: "no"   -> abort
[await_target]   (only when SPAWNER_SANDBOX_IMAGE is configured; otherwise skipped)
  app : "run <dir> on the host, or in a sandbox?"
  user: "host"                      -> Session.Target = host   -> [await_attach]
  user: "sandbox"/"container"/"isolated" -> Session.Target = sandbox -> [await_attach]
    (no sandbox image configured -> target is always host, this state is skipped)
[await_attach]
  app : "found <name>. want to attach?"
  user: "yes"  -> persist session + attach -> [attached]
  user: "no"   -> persist session (ready to attach later), stay [idle]
  user: "cancel" -> abort, nothing persisted   (cancel works from any state)
```

`browseInto(dir)` prompts for a subfolder when `dir` is a **root** or a **namespace** (a non-repo
dir that contains git repos, e.g. `~/git/SparkyFitness`); otherwise `dir` is treated as the target
(a git repo like `~/git/drat`, or a plain service dir like `/data/jellyfin`). This keeps you from
having to spell exact paths while still stopping at the right level.

Per-segment matching drops filler words (the, a, repo, folder, …) and fuzzy-matches the remaining
terms against a directory's immediate children (separator- and camelCase-aware), taking the best
match. Roots come from `SPAWNER_ROOT` — a `:`-separated list (e.g. `/home/bam/git:/data`).

**Transcription-error tolerance.** Root and folder matching use edit distance, so whisper slips
like "get" → `git` or "personel" → `personal` still resolve. (`projects.Levenshtein` /
`FuzzyEqual`, used in `matchRoot` and `Rank`.)

**Fuzzy-match confirmation.** When navigating to a **leaf** project lands on a folder whose name
carries a token you never said — the matcher stretched "mail" onto `mail_play` because no `mail`
folder exists — the flow doesn't silently attach; it asks `[await_confirm]` ("did you mean
mail_play?") first. Exact names and multi-word names you spoke in full ("mail play" → `mail_play`)
skip the confirmation. Only leaf commits confirm; a stretch onto a root/namespace just keeps
browsing, so it re-prompts anyway.

**Entry points.** The whole path can ride on the command:
- `spawn a session in git personal` → jump to `~/git/personal` and browse it (skip the "where?" prompt).
- `spawn a new project in git personal` → jump there and go straight to `[await_newname]`:
```
[await_newname]
  app : "what's the new project called, in personal?"
  user: "<name>"  -> mkdir ~/git/personal/<name> -> [await_attach]
```
Location resolves the same root+segment way as interactive navigation (so it's fuzzy too).

Edge cases:
- Root not recognized -> "start with git or data, bud."
- Nothing matches a spoken subfolder -> offer to create it under the current folder.
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

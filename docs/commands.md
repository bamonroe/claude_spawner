# Command grammar

The authoritative "hey buddy" command vocabulary and dialog flows. Both the Android app and the
Go server reference this. When you change a command, change it here first.

## Conventions

- Every control command is prefixed with the wake word **"hey buddy"** or **"hey bud"** (the
  server also accepts common whisper mishearings: "hey body", "hey buddie", "hey budy").
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
| `spawn`         | "spawn a new session" / "spawn a session in git personal" / "spawn a new project in git personal" | Starts the spawn dialog (see below). An inline location after "in"/"at" jumps straight there; "new project" switches to create mode. |
| `attach`        | "attach to `<name>`"                     | Attaches; subsequent speech is dictated           |
| `detach`        | "detach" / "stop dictating"              | Leaves passthrough mode                            |
| `list`          | "list sessions" / "what sessions"        | Reads back known sessions                          |
| `kill`          | "kill session `<name>`"                  | Confirms, then kills the tmux session             |
| `status`        | "what's the status" / "what's it doing"  | Snapshot of the attached session's recent output  |
| `read_last`     | "read last" / "read last 3" / "read the last two" / "say that again" / "repeat that" | Re-reads aloud (TTS) + scrolls to the last N Claude replies in the current session (N defaults to 1; digit or number-word). |
| `clear`         | "clear" / "clear context" / "clear session" / "clear the context" / "reset context" / "start fresh" / "wipe context" | Rotates the attached session's Claude context to a fresh `session_id`, so the next dictation replays **no** prior history (no re-read, no re-billing of the whole transcript). The old transcript is **kept on disk** and still shows in `history`. Deliberately NOT matched: "clear history" — clear never deletes. No-op if no turn has run yet; refused while a turn is in flight. |
| `cancel`        | "cancel" / "never mind"                  | Aborts the current dialog                          |
| `stop`          | "hey bud stop" / "stop talking" / "quiet"| Barge-in: stops TTS everywhere; never dictated. Also, pressing push-to-talk stops speech client-side. |
| `abort_turn`    | "stop the turn" / "cancel the turn" / "abort" / "stop working" | Cancels the running Claude turn (kills the child). Distinct from `stop` (TTS) and `cancel` (discard a composing message). |

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
[await_create]
  user: "yes"  -> mkdir <browse>/<name> (validated against roots) -> [await_attach]
  user: "no"   -> abort
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

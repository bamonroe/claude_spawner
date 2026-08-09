# claude_spawner

A voice-driven remote control for [Claude Code](https://claude.com/claude-code).

Speak to an **Android app**, and it relays your voice to a **server** on your machine that spawns
and manages **Claude Code sessions**, driving them headless. The app is a hands-free passthrough:
say a command and it runs; attach to a session and your dictation goes straight to Claude, with
replies streamed back and read aloud.

**Quick start:** the first-run guide — prerequisites, one-command bring-up, SSH key authorization,
and getting a client — is [`deploy/README.md`](./deploy/README.md). Clients build per
[`android/README.md`](./android/README.md) (APK) and
[`docs/web-client.md`](./docs/web-client.md) (browser).

## How it works

You start every command with the wake word **"hey buddy"**:

```
You:   "hey buddy, spawn a new session"
App:   "ok bud, where do you want it?"
You:   "in data claude underscore claude"
App:   "ok, made that directory. want to attach?"
You:   "yes"
App:   (attached — now everything you say is dictated to Claude Code)
```

- **Speech-to-text** runs on the **server** (Whisper); the **wake word** "hey buddy" is matched
  **in that transcript, server-side** — there is no on-device keyword engine.
- The **server** drives Claude Code **headless** (`claude -p --output-format stream-json`, with
  `--dangerously-skip-permissions`). A session is a durable `session_id` on disk, reattached each
  turn with `--resume`, so replies come back as clean structured text — never scraped from a
  terminal UI. (Design notes: [`CLAUDE.md`](./CLAUDE.md), [`docs/architecture.md`](./docs/architecture.md).)
- While **attached**, your speech is dictated into the session and Claude's reply is streamed back
  to the phone (display + text-to-speech). You can also `claude --resume <id>` in a real terminal to
  watch or take over the same session — the server detects this and warns rather than driving it
  concurrently.

## Stack

| Part        | Choice                                                              |
|-------------|---------------------------------------------------------------------|
| Server      | **Go** — WebSocket gateway, headless session manager, Whisper glue  |
| Android app | **Kotlin** — VAD-gated audio capture, TTS, WS client                |
| Wake word   | **Server-side** — matched in the Whisper transcript (`command.StripWake`) |
| STT         | **Server-side Whisper** (wake word + dictation both matched on the server) |
| Sessions    | **headless `claude -p` (stream-json)**, durable via `session_id` on disk |
| Conflict check| **tmux** inspected to detect a `claude` a human has open in a pane      |

## Reserved commands

All prefixed with **"hey buddy"**:

- `spawn a new session` — one-shot when you give enough ("new session called bugfix in data on zen
  with sandbox profile"), else an interactive dialog. Name, location, provider, and profile are all
  optional; unspoken ones default (home directory, Claude, your default profile)
- `attach to <name>`
- `detach`
- `list sessions`
- `kill session <name>` — drops the session and wipes its transcripts. If the machine it ran on is
  unreachable, the delete still completes **immediately**: the session goes away now, and the
  leftover remote files are swept up automatically the next time that host is back online.
- `rename to <name>` / `call this <name>` — rename the session you're attached to
- `what's the status` / `what's it doing`
- `read last` / `read last 3` — re-read Claude's recent replies aloud
- `clear the context` — start Claude fresh **without** losing your history (see below)
- `compress the context` — like `clear`, but carries a **summary** forward (see below)
- `list models` / `use model <number>` — list the AI's models and switch by number (see below)
- `scratch on` / `scratch off` — **scratch mode**: while detached, hear each transcription read back so you can test how well Whisper is hearing you (see below)
- `set target <host>` / `set directory <path>` — where work happens: the host new sessions spawn on and shell commands run on, and the default directory a spawn lands in (see below)
- `get target` / `what is the directory` — read the current setting back
- `summary only` / `speak everything` — **summary-only speech**: on a long, multi-step turn, read aloud only the **final result**; each intermediate step plays a soft beep instead of being spoken (see below)

Anything spoken **while attached** that isn't a reserved command is dictated to the session. When a
command fails (a bad path, a name that's taken, a session live in a terminal…), the server speaks a
plain-language reason instead of failing silently.

**Wake and end tokens (Settings → Commands).** The two spoken tokens that bracket a command live on
the Commands settings page. The **end token** (default "beep") commits a hands-free message. The
**wake token** field lets you add your own wake word(s) — accepted *alongside* the built-in "hey
buddy" (blank keeps "hey buddy" only). It's **comma-separated**, so you can list several variants
("hey buddy, hey bud, ok buddy") and have any of them fire — useful because Whisper mis-hears the
wake phrase in a noisy room. Pick words Whisper transcribes cleanly: a custom word has no curated
mis-hear alias list the way "hey buddy" does, though the server does bias transcription toward it.

**Speech gate for noisy rooms (Settings → Commands).** In hands-free mode with a lot of ambient
chatter — other people, a radio, a recording — you don't want all of it dictated into your session.
Turn on **Require a speak token** and set a **speak token** (e.g. "take a note"). The token then
becomes the **front door to the whole pipeline**, not a filter applied at the end: until you say it,
each clip is only scored for the phrase and *nothing is kept* — no audio buffered, no live draft, no
transcript bubble on the phone, and no second (accurate) Whisper pass. Say it and capture starts,
running exactly as ungated hands-free does until your end token, which commits the message and closes
the gate again: "take a note, fix the parser bug, beep".

Because the gate is the front door, **everything is gated** — including commands ("take a note, hey
buddy attach to spawner, beep") and including barge-in. There are no side entrances: while the gate
is shut nothing at all is matched, so stopping runaway speech means "take a note, hey buddy stop".
A gate that opens but never hears an end token re-closes itself after 90 seconds, so a misfire can't
leave the mic live on the room. Push-to-talk and mic lock are unaffected — a held mic is already an
explicit front door.

**You can use the same phrase for both brackets.** Set the speak token and the end token to the same
word and it works as you'd say it: "pickle, fix the parser bug, pickle". The first occurrence opens
the gate and is stripped off the front of the message immediately, which leaves the second free to
match as the end token. The one combination that can't work is the same *trained detector model*
bound to both — the sidecar only reports that the sound happened, not which bracket it was — so in
that case the opening clip's hit counts as the opening, and the close comes from the next clip or
from a second occurrence in the transcript.

**A pill above the input bar shows the gate's live state** — "gate shut — say the gate phrase" or
"gate open — capturing" — so you can see at a glance whether the machine is listening to you or
dropping what it hears. It appears only while the gate is on, in both the app and the web client, and
the server pushes every transition (including on connect), so it can't drift out of sync.

The trade-off to know: a *missed* gate now loses the audio too, so there's no transcript showing what
was heard (the server logs `speech gate:` lines when it opens or times out). Leave the switch off (or
the speak token blank) to dictate everything as before. The speak token is comma-separated, so you
can give it a couple of variants.

**Setting the target and directory by voice.** Two shared settings decide *where* work happens, and
both can be spoken: **"hey buddy, set target `<host>`"** picks the machine — used both as the host new
sessions spawn on and as the host shell commands run on — and **"hey buddy, set directory `<path>`"**
picks the folder a spawn with no spoken path lands in. Values are checked before they're stored: a
target must name a host you've configured (fuzzily, so a mis-heard name still lands, otherwise the
known hosts are read back to you), and a directory is resolved segment-by-segment against the
target's real filesystem over SSH and saved as the resolved absolute path. Both persist across server
restarts and sync to every client. Ask for either back with **"hey buddy, get target"** or **"hey
buddy, what is the directory"**; an unset target reads back as `localhost`.

**Pre-configured shell commands (Settings → Shell commands).** While you're detached from every
session, a **shell token** (bind the phrase in Settings → Spoken tokens, action "Shell") opens a
second parsing surface: the rest of what you say names one of the commands listed on this screen, and
it runs over SSH on the target host with its output spoken back in full. The catalogue *is* the
safety boundary — only what's listed here can ever run, so spoken shell is never arbitrary.

Each entry has a **spoken alias** (what you say), a **command template**, and optionally a working
**directory** and a **host** to pin it to (blank = wherever the shell target is set). The template
takes spoken arguments: `$1`…`$9` are the first nine spoken words after the alias, `$*` is the rest
space-joined, and `$$` is a literal dollar. Substituted values are shell-quoted, so an argument can
never sneak in an operator or a second command. The command is announced as it starts and then its
whole output is read back, stdout and stderr together; a silent command says it finished with no
output, and anything still running after a minute is reported as timed out. Add, edit and delete are live: the list is stored on
the server and shared across every device, so a command you add on the phone is there in the web
client too.

If what you said doesn't line up with the catalogue, the reply tells you how to fix it by ear: with
nothing configured it says so, an alias it can't find is repeated back with the closest one or two
configured names ("did you mean disk space?"), and a template whose `$1`…`$9` you didn't fill asks
for the missing value instead of running the command with a blank in its place.

**Wake/end-token detection backend (Settings → Commands).** By default the live hands-free wake and
end tokens are recognized by string-matching the fast Whisper transcript — always available, no extra
service. Turn on **Use dedicated wake-word detector** to instead score the purpose-trained LiveKit
detector sidecar (the server's `SPAWNER_WAKEWORD_URL` service — see the wake-word detector notes in
[`TODO.toml`](TODO.toml)). Leave it off unless that sidecar is running and you've validated the model;
the server treats any client that doesn't opt in as Whisper.

**Adapting to background noise (Settings → Audio).** The mic threshold slider sets how loud a frame
must be to count as speech. With **Adapt to background noise** on (the default), that number isn't a
fixed gate: the app continuously measures the room's ambient noise floor during silence and lifts the
speech bar to sit above it, so it self-calibrates as you move between a quiet room and a noisy one —
the slider then acts as a *minimum* rather than an absolute cutoff. Turn it off to go back to a fixed
threshold you set by hand. When adapting is on, the **Noise margin (×)** dial sets how far above the
measured noise floor your voice has to rise to register (default 2.5×) — raise it in a loud
environment (a train, wind, an engine) so the steady roar stops tripping the mic, lower it in a quiet
room to catch softer speech. Two more dials shape when a phrase starts and ends: **Speech to start**
is how long a sound must hold above the bar before capture begins (longer rejects brief clicks and
blips), and **Max phrase length** is a hard safety cap that ends a clip that many seconds after it
starts even if silence is never heard — the backstop for continuous wind or road noise that never
dips low enough to close the phrase on its own. Separately, **Headset noise suppression** runs the
phone's built-in noise suppressor on the Bluetooth-headset capture path too (it's off there by
default because that filter is tuned for a near mic and can attenuate a voice picked up from across
the room); switch it on if steady background noise on your headset is getting transcribed.

**Server-side denoise (Settings → Audio).** The dials above decide *when* a clip is captured; they
can't clean up a clip that was already noisy when the gate opened. For that there's **Server-side
denoise**, which runs each captured clip through the server's DeepFilterNet noise remover before it's
transcribed — a far stronger scrub of steady wind, road, engine and fan noise than the phone's own
suppressor, because it's a full neural model running on the server's GPU. It only appears when the
server has a denoiser configured (`SPAWNER_DENOISE_URL`, pointing at the `deepfilternet` service in
`/data/speech_services`); otherwise the toggle explains that this server has none and clips go through
unfiltered. It's off by default and a per-device choice, because denoising adds a little latency to
every phrase — leave it off in a quiet place, switch it on for the train, the car, or a windy bike
ride. When it's on, the **Noise removed (dB)** slider caps how much noise the model may subtract:
higher strips more (cleanest in heavy noise), lower is gentler and keeps the voice more natural, and
the top of the range is effectively full strength. The scrub happens at one seam on the server, so it
covers push-to-talk, the hands-free draft, the end-token detector and the final accurate transcribe
alike; if the denoiser ever errors the server quietly falls back to the original clip rather than
dropping the turn.

**When the end token misfires.** If "beep" isn't caught and the clip keeps growing, whatever you
say next still lands in the same message — so you can just keep issuing commands: the server splits
a committed message on **every** "hey buddy" and runs them in order ("hey buddy list, hey buddy
detach"). Your leading dictation goes through in spoken order too — it's sent to the session before
the commands run, so "<something to say> hey buddy detach" reaches the session before the detach
takes it away. And **"hey buddy, cancel"** (or "cancel that") is a reset point — it scraps everything
before it (the dictation and any earlier commands), while commands after it still run, so you can
self-correct mid-utterance. End on a cancel with nothing after it and the whole message is scrapped.

**A dedicated end-token detector (optional).** Matching the end token in Whisper's transcript is the
main source of *missed* commits — Whisper mishears "beep beep" and your message never sends. Point
`SPAWNER_WAKEWORD_URL` at the `spawner-wakeword` sidecar (a small, purpose-trained keyword-spotting
model) and the server instead scores each clip's audio directly for the wake and end tokens — far
fewer misses on short tokens. It's a **gate, not a transcriber**: when it detects the end token, the
whole utterance is still handed to accurate Whisper for the real parse, so nothing about your command
text changes. `SPAWNER_WAKEWORD_THRESHOLD` (default `0.5`) tunes how eager it is — lower it toward the
models' optimal ~`0.04`–`0.07` to trade a few false triggers for near-zero misses. Leave the URL empty
and detection falls back to the Whisper string-match; if the sidecar is unreachable mid-turn, the
server degrades to that fallback automatically rather than dropping the command.

**Training the detector's models is out of scope for this project.** claude_spawner only *consumes* a
finished wake/end-token model (via the `spawner-wakeword` sidecar and `SPAWNER_WAKEWORD_URL`). Building,
augmenting, and retraining those models — including collecting real-voice samples to close the
synthetic-to-real gap — lives in a separate training project at `/data/livekit_training`. Point this
server at whatever model that project ships.

**The mic button (hold to talk).** With the box empty, **press and hold** the mic to record; release
to send. The hold is *sticky* — it keeps recording even if your finger drifts off the small button —
but two deliberate drags end it early: drag **up** past the track that appears (about 120 dp) to
switch into **hands-free**, or drag **left** the same distance to **discard** the clip. If a long
hold ever cuts on its own, turn on **Settings → Debug** (see below) to see the drag thresholds drawn
as boxes and log why each hold ended.

**The radial session palette (double-tap the transcript).** Double-tap anywhere on the chat
transcript and a ring of session buttons blooms out from where you tapped (clamped so it stays
on-screen). The double-tap only *observes* your touches, so it never steals them: a double-tap on a
bubble, a link, or the jump-to-latest button still does that thing *and* opens the ring. Dismiss it
by tapping the dimmed background, pressing **back** or **Escape**, or tapping any button in it.

It works the same in the **browser client**: double-*click* the transcript opens the ring at the
pointer, and since a browser has no back button, **Escape** is the way out (the dimmed background
still works too). The ring, its attach-history order and the mic-lock centre are one shared
implementation, so the two clients can't drift apart.

**The ring is configurable, and it has submenus (Settings → Radial menu).** What the ring shows
is a **tree** you edit yourself: each entry is either a **built-in action** (lock mic, new session,
swap, detach, hands-free, stop speaking, check usage, settings, close), a **submenu**, or a **live
ring** built from app state (**Sessions** and **Commands**). Tapping a submenu opens it **in place** — the ring
does not move, re-bloom, or change geometry; only its contents swap. Inside a submenu the centre
button becomes **Back**, and the dimmed background, **back** and **Escape** pop one level instead of
closing, so the ring only ever disappears from the top level. Nesting is unlimited.

The centre button of the top-level ring is bound to an action of your choice (the mic lock by
default). A live ring can either be a **button that opens the sessions submenu** (the default) or be
**spliced straight into the ring** it sits in — that switch reproduces the flat session ring the
palette had before submenus existed. The editor drills down exactly like the menu does, and
**Reset to default** puts the stock menu back. Config is stored per-client and shared by the Android
app and the browser client.

The **Sessions** ring holds up to **eight** sessions, ordered by **attach history** — the one you were on most
recently sits straight up from the centre, and the rest run clockwise into the past, so switching
back and forth is always the same short flick. The session you're currently attached to is left out
(you're already there), as is anything the server no longer knows about, and the list survives app
restarts. On a fresh install, with no history yet, the ring falls back to last-active order so it's
never empty. Each slot is labelled with the session's name, or the last part of its directory if it
hasn't got one.

The **Commands** ring holds the same curated, argument-free commands as the swipe-up command tray
(chosen in Settings → Commands), each as its own slot — so a single command is one flick away without
opening the tray. Tapping a slot sends that command's "hey buddy …" phrase to the attached session
and closes the ring; while disconnected the ring is empty. Like Sessions, it can be a submenu button
or spliced flat into its parent ring, and it shows at most eight slots.

The most-recent-first session order also drives the left-edge swap gesture. The
**centre** of the ring isn't a session — it's the mic lock.

**Mic lock (hands stay free, wake word stays off).** Holding the mic gets tiring for a long
dictation, so the **radial palette** (double-tap the transcript) has a **centre button that locks
the mic open**: tap `lock mic` and recording starts and *keeps* running with no finger down.

The palette **stays open** while locked, and becomes the recording control itself: the ring of
sessions gives way to just two buttons under your thumb — a red **mic** in the centre that **sends**
the clip, and an **✕** directly above it that **cancels** it. Nothing implicit closes the palette
while it's locked (the dimmed background, back and Escape are all inert), so a clip only ever ends
on a deliberate tap. A red **mic locked** line still appears above the message box and the mic
button in the corner goes red, so tapping that mic sends too. This is *not* hands-free: there's no wake word and no
voice-activity detection, it's exactly the ordinary tap-to-speak capture held open, so the whole
locked stretch is one clip.

Locking needs the same conditions as a hold — connected, and hands-free off — and the lock releases
itself rather than recording into the void: **leaving the app** (or hiding the browser tab) ends the
clip and sends it, **switching sessions** ends it first so it lands in the session you actually spoke
into, and **losing the connection** or **turning hands-free on** drops the clip (it could not be
delivered as recorded). While locked, hold-to-talk is disabled so the two paths can't fight.

**The ring shows which sessions want you.** A palette slot turns **orange** under exactly the same
rule as the sidebar's session cards: that session is thinking right now, or it's holding output you
haven't seen yet (the session you're attached to is never orange — you're already looking at it).
So a glance at the ring tells you where to switch without opening the sidebar. Both surfaces read
one shared definition of the cue, so they can't disagree.

**Debug overlays (Settings → Debug).** A developer toggle, off by default. It draws translucent boxes
over the normally-invisible push-to-talk zones — the red **discard** zone (drag left) and amber
**hands-free** zone (drag up) — with a live readout of your finger's drift and hold time while you
hold, and logs each hold's end reason and drift to logcat (tag `PTT`). Meant for diagnosing a fiddly
hold-to-talk, not everyday use.

**About / build stamp (Settings → About).** Shows the app version and the exact git commit the
installed bundle was built from — short hash + branch, the full commit, and the build time — so you
can tell at a glance which build is running on any given device (phone vs. tablet vs. browser). The
commit is stamped in automatically at build time by a Gradle step that reads it straight from `.git`,
so it can never drift from the code that was actually compiled. The page is shared UI, so it appears
in the browser client too.

**Without your voice:** swipe up on the message box — or tap the **chevron handle** just above it —
for a **command tray** of tap buttons, one per command you've chosen. The tray is **curated in
Settings › Commands**: each command is a **card you tap to expand**, and an expanded card lets you
**add it to (or remove it from) the tray** as well as add spoken aliases. It starts seeded with every
argument-free command (`detach`, `clear`, `compress`, `status`, `usage`, …); prune it to just the
ones you reach for, or empty it entirely. (Commands that take a spoken argument — `attach`, `kill`,
`spawn` — can't be one-tap tray buttons.) Open the **sessions drawer** with the ☰ menu or by swiping in from
the left edge (just inside the edge — the very edge is Android's back gesture). The session list
**auto-refreshes each time the drawer opens**, and you can **pull down on the list** (or tap
**Refresh**) to re-scan at any time. See [`docs/commands.md`](docs/commands.md).
Swiping right-to-left on the chat jumps back to the previous focused session immediately; the app
then silently syncs that focus to the server so the next dictation and other clients agree.

Each session is shown as a **card** with its name, AI backend/model, and a **sandbox** badge when
it runs in a container. The list is **colour-coded and sorted by attention**: the session you're
**attached to** is tinted **purple** and pinned to the top; sessions that are **thinking** (a turn
running) or hold **unread output** (new activity landed while you were attached elsewhere) are
tinted **buddy orange** and sorted next by most-recent activity; everything else stays neutral,
sorted alphabetically. A session clears its orange the moment you open it, and a fresh launch
starts everyone neutral (nothing is marked unread until new output actually arrives). A **▶ play
button** on the right of each card **attaches to that session directly**, no expanding needed.
**Tap the card** itself to
**expand it in place** (tap again to collapse), revealing its **directory path** and three actions:

- **Open** — attach to the session (the same as tapping a row used to do).
- **Edit** — rename it, and (when the server advertises more than one backend) **switch its AI
  agent + model**. Changing only the model keeps the conversation; **switching the backend rotates to
  a fresh conversation** on the new AI (Claude and Codex transcripts aren't interchangeable on disk),
  but the context is **carried across**: the server seeds the new backend's first turn with a recap of
  the recent conversation, so the new AI continues where the old one left off. The badge flips right
  away — reading the outgoing transcript for that recap happens in the background, and only the
  session's next turn ever waits on it. The old transcript also stays on disk **and stays in the chat log** — the
  messages from before the switch remain in the scrollback, each read back with the backend that wrote
  them, so switching AIs never blanks your history. The dialog still warns you before you commit.
- **Delete** — permanently remove the session's transcript(s) (with the same confirmation as before).

### Transferring files to and from a session

To the **left of the message box** is a transfer button (📎). Tap it to **upload** or **download**
files over the same authenticated WebSocket — no separate share sheet or `scp`. Both directions
support **selecting multiple files** at once.

- **Upload:** pick one or more files on the phone (the system file picker is multi-select), then
  choose a single destination directory on the session's host — the picker opens at the **session's
  own directory** and browses that host's filesystem (the same host-scoped browser the New-session
  picker uses, over SSH). All picked files are written there, and the message box is **prefilled**
  with `look at the file at <path>` (paths comma-joined when you sent several) — *not sent*, so you
  can edit or add to it before dictating/hitting send.
- **Download:** the reverse — browse the host's filesystem starting at the session's directory (files
  are shown alongside folders, each with a checkbox), **tick one or more files** (the selection
  persists as you navigate between folders), confirm, then choose where to save each on the phone.

Bytes travel base64-encoded in one message per file each way, capped at 64 MiB per file. Multi-file
transfers are simply several of those messages. Because the transfer runs on the session's host over
SSH, an upload lands on the very machine the session runs on (loopback for a local session), exactly
where Claude will look for it.

### Offline transcript cache

The app keeps a **local, on-disk copy of each session's chat history**, so you can scroll back through
big chunks of a conversation even with no connection — and switching between sessions doesn't re-download
what you've already seen. Every time the app connects it asks the server for a lightweight **digest** of
each session (a message count plus a content hash — no message bodies), and compares it against the cached
copy. If the hash still matches, clicking into that session shows the cache and **transfers nothing**. If
the hash changed, only that session is refetched (and if it merely grew, just the new tail). A `compress`
rewrites the transcript, which changes the hash — the app notices and pulls a fresh copy rather
than stitching a stale one. (A `clear` doesn't: it leaves the rendered log byte-identical, so the
app keeps its cache and the freshness check comes back `unchanged`.) The cache lives under the app's private storage and survives restarts; the
hash is opaque to the app, so this stays correct without the phone and server having to agree on how it's
computed.

The cache is **pre-warmed at launch**. Right after startup the app decodes the most recently used
cached transcripts into memory in the background, so the first tap into a session renders its chat
instantly instead of showing an empty screen while the file is read and parsed. The in-memory set is
bounded (least-recently-used sessions fall out first); anything evicted is still on disk and loads on
demand.

The cache is also **refreshed in the background** while you work. A prefetcher quietly issues the
same history request a switch would for the most recently active sessions you are *not* looking at —
prioritized by the server's connect-time digest sweep, so only sessions whose transcript actually
changed are fetched. At most two requests run at once, it pauses entirely while a turn is streaming
**or while any history request you are actually waiting on is in flight** (the viewed session's
refresh, its attach page, a scroll-back or a reconnect gap-fill), and it can never move your view: by the time you tap a session, its chat is usually already warm and
current. The per-attach `have_hash` freshness check stays the authority; the sweep only decides what
is worth fetching early.

The cache **prunes itself**. A session deleted on the server would otherwise leave its transcript on
the phone forever, since nothing else deletes cache files. Each time the session list refreshes, the app
sweeps its cache directory and drops entries for sessions that no longer exist. Because you may use more
than one server, and any given list only speaks for the one you're connected to, absence is treated as
evidence rather than proof: an entry is deleted only after it has gone unseen for **two weeks**, so a
session on a machine you simply haven't opened lately survives as long as you come back within that
window. The sweep is throttled to once an hour and runs off the UI thread.

The **session list itself is cached** the same way: the last set of discovered sessions is written to
disk on every connect, so a fresh launch shows the sidebar populated (and lets you click into any
session's cached transcript) **before — or entirely without — a server connection**. It's refreshed
wholesale the moment the server's discovery sweep comes back. Live-only flags (a session being active or
mid-turn) aren't cached, since offline nothing is running; they light up again on connect.

### Clearing vs. compressing context

Every dictated turn resumes the session with `--resume`, so Claude re-reads the whole conversation
each turn — which makes a long session progressively more expensive.

- **"hey buddy, clear the context"** rotates to a fresh `session_id`: the next turn starts Claude
  with empty context (no re-read, no re-billing). Nothing is deleted — the old transcript stays on
  disk and still scrolls back in the app; Claude just stops seeing it. Use it when starting
  unrelated work in the same directory. It is **instant on screen**: a clear changes nothing on
  disk (it only retires the old id onto the session's chain), so the server marks the rotation
  "preserved" and the app re-keys the chat it already has onto the new id instead of blanking it
  and re-downloading the conversation.
- **"hey buddy, compress the context"** is the `/compact` analogue: the server has Claude summarize
  the conversation, rotates to a fresh `session_id`, and prepends that summary to your next
  dictation — so Claude keeps a compact recap instead of the full transcript. Costs one model turn.
  Use it to keep going on the same task while trimming cost.

**Automatic compression** (Settings → Server) runs that compress for you. Set a token limit (in
thousands) and turn on either of two triggers that share it — the trigger is server-side, so it
fires even when the app is detached, and the preference is global (one limit for all sessions):

- **Warm compress** — once a session's context grows past the limit, fire a compress in the last
  ~15 seconds of its ~5-minute warm prompt-cache window, so the summary turn reuses the still-warm
  cache instead of paying a cold context rebuild later. Opportunistic: it waits for that edge.
- **Auto compress** — compress the moment an idle session crosses the limit, without waiting for the
  warm window. Immediate (it may pay a cold cache read); wins over warm compress if both are on.

The compress summary keeps your **most recent messages in near-verbatim detail** and squeezes older
history harder, so the active working context survives compaction.

### Scratch mode: testing transcription

**"hey buddy, scratch on"** turns on a transcription-quality test loop. While you're **detached**
(no session attached), the server takes each utterance it recognizes and — instead of doing nothing
with it — reads it straight back to you via TTS, so you hear exactly what Whisper heard. It's a fast
way to gauge how well the current model is transcribing you, or to compare models after changing the
full/quick picks. **"hey buddy, scratch off"** stops it; a bare "scratch" toggles. It only echoes
while detached, so it never interferes with a live session — attach and your speech dictates as
usual. Reserved commands still work in scratch mode (a detached utterance is parsed as a command
first), so speak ordinary sentences to exercise the transcriber.

### Summary-only speech: don't read every step of a long turn

On a long, multi-step turn — a big investigation with many subagents — Claude streams each
intermediate step as it happens, and normally the client **reads every one aloud**. When you're
just waiting for the final answer, that's a lot of narration you don't need. **"hey buddy, summary
only"** switches to summary-only speech: the client reads aloud **only the final result** of a
turn, and plays a soft, warm **beep** in place of speaking each intermediate step — so you still
hear that work is happening (you're not in the dark), without the play-by-play. Every intermediate
message that lands in the chat gets its own beep — streamed prose, changed-files and diff notes, a
subagent finishing — so the audible count matches what you see; only the turn's final spoken result
doesn't beep. Everything is still shown on screen as usual; only the *speaking* changes. **"hey
buddy, speak everything"** turns it back off ("summary only off" works too).

The same toggle is the **Summary only** switch on the **Audio** settings page. The setting lives on
the client (persisted per device), so the voice command and the switch stay in lock-step and the
server keeps no per-connection state.

**Speak initial replies** (an integer field under the switch, default 0) refines this: in
summary-only mode, the **first N streamed replies of each turn** are spoken aloud like normal, and
only the *remaining* intermediate steps beep — the turn's final result is always spoken either way.
So N=1 speaks a turn's opening reply (usually Claude's plan or first take) and its final summary,
beeping the working steps in between. It's a per-turn count that resets at the start of every turn,
and it has no effect when summary-only is off (everything is already spoken). The field only appears
while **Summary only** is on.

The beep is a low, round sine tone with a smooth envelope —
deliberately unlike a sharp notification chime — and in hands-free mode it plays through the
echo-cancelled voice path so the open mic doesn't hear it.

### Server voice: Kokoro speech synthesis streamed to the device

When the server is configured with a resident Kokoro TTS server (`SPAWNER_TTS_URL`; the `kokoro`
service in the `/data/speech_services` stack), replies can be spoken with a **neural server-side
voice** instead of the
device's built-in text-to-speech. The decision stays on the client — the **Server voice** switch on
the **Audio** settings page (on by default, active only when the connected server offers TTS): for
each reply the app sends the text up as a `speak` request, the server synthesizes it with Kokoro and
streams the audio straight back down the WebSocket, and the app plays it as it arrives. Nothing is
synthesized for muted or summary-only-beeping clients, since they never ask. If the server refuses
or synthesis fails, that utterance is read by the device's own voice automatically — the fallback
needs no toggling. Barge-in ("hey buddy stop", push-to-talk) halts server-voice playback exactly
like local speech — and it also tells the server to abort the in-flight synthesis, not just the
local playback. The web client has the same switch and fallback — it asks for mp3 and plays the
clip via Web Audio (the phone streams raw PCM) — and on-device speech remains the whole story when
`SPAWNER_TTS_URL` is unset.

Kokoro ships dozens of voices, and each device picks its own: a **Voice** dropdown under the Server
voice switch lists the server's catalogue (relayed live from Kokoro), with the `SPAWNER_TTS_VOICE`
default as the first entry. Picking a voice speaks a short preview in it, and the choice rides each
synthesis request from that device — nothing is stored server-side, so your phone and your browser
can sound different.

### Detached background jobs that outlive a turn

Each turn drives a **fresh** headless `claude` process (resumed from disk), so Claude's own
`run_in_background` can't help with something that should keep running *after* the turn ends: the
background process shares the turn's process group and output pipes and is torn down when the turn
finishes (over SSH the channel closes and the group is killed), and even if it survived, the next
turn's brand-new `claude` has no in-memory handle to poll it.

The server provides a **`spawner-job`** wrapper for this. Claude is told, once per context, to start
any long-running command (a build, a dev server, a watch, a long test run) with
`~/.spawner-jobs/spawner-job start '<command>'` instead of `run_in_background`. The wrapper launches
the command **fully detached** — its own `setsid` session, `nohup`, stdin from `/dev/null`, and
stdout/stderr redirected to a log file — so nothing about the turn's teardown can reach it. Each job
is recorded in an on-target registry **keyed by the session's working directory** (so it survives a
`clear`/`compress` that rotates the session id).

The server reconciles that registry on three triggers — at every turn boundary, when a device
attaches, and on a **background ticker that runs even while you sit idle** (every ~12 s). When a job
has finished it injects a short, length-capped completion note — the command and a tail of its
output — ahead of Claude's next turn, so **Claude is told the job is done** and can react. Crucially,
if a job finishes while you're just waiting for it (not typing or speaking), the idle ticker catches
it and, if a device is attached, **drives an autonomous turn so Claude tells you out loud right
then** — you don't have to say something else first to unstick the notification. With nothing
attached, the note simply waits and surfaces on your next message or reconnect, so a finish is never
lost. Set **`SPAWNER_EAGER_NOTIFY=true`** to make the idle ticker fire that follow-up turn **even
when no device is attached** — the job's next step runs the moment it finishes rather than waiting
for you to revisit the session, which keeps the agent from sitting idle for the gap and makes the
turn far likelier to land inside the token-cache window (the spoken reply is buffered and delivered
when you next attach). The default (`false`) keeps the conservative behavior above and never narrates
to an empty room.

An eagerly-narrated turn speaks to a session nobody is on, so the app would otherwise learn nothing
until you opened that session. To close that gap the server also pushes a lightweight **notice** to
every device that is *still connected but looking at another screen*: the app shows an orange banner
above the chat with the session's name and a one-or-two-sentence summary of what finished, and
tapping it jumps straight to that session (the ✕ dismisses it). The banner is deliberately not a
chat message — it's never filed into the transcript you're reading and never read aloud — and there
is at most one per session, so a busy session replaces its banner rather than stacking them. Devices
already attached to that session don't get it; they received the real reply. Waking an app that is
**fully closed** still needs push infrastructure that doesn't exist yet. Because the ticker re-polls on a schedule, a momentary SSH hiccup no longer silently swallows a
completion — the next tick just tries again. Claude can also check progress itself at any time with
`~/.spawner-jobs/spawner-job list` / `tail <id>`. Reconcile and staging failures are swallowed and
never block a turn. Two caveats: a **sandbox** session's jobs live only as long as its container —
removing or recreating the container loses them — and the registry is keyed by the exact working
directory, so a job Claude launches from a *subdirectory* of the session's dir won't be picked up by
the per-dir reconcile.

You can also inspect and control these jobs by voice: **"hey buddy, list jobs"** speaks the attached
session's jobs (numbered, each marked running or finished), **"job status"** gives the quick
running-vs-finished count, and **"kill job 2"** stops one by its number (taking its whole process
group down). The number is required, so it's never confused with killing a session or aborting the
turn.

This isn't left to Claude remembering the instruction. The server also installs a **Claude Code
PreToolUse hook** (injected at launch via `claude --settings`) that runs on every `Bash` tool call:
if the call asks to run in the background, the hook **transparently rewrites it** to run detached
through `spawner-job start` instead — it doesn't cancel anything, so from Claude's side the same Bash
tool just runs the wrapped command, with no retry and no confusion. (The rewrite uses the hook's
`updatedInput`, which replaces the tool's arguments before it runs; `jq` shell-quotes the original
command so it reaches the wrapper intact.) Hooks fire even under `--dangerously-skip-permissions`, so
a background command can't slip through the old, fragile way — the survival guarantee is enforced by
the harness, not by Claude's cooperation. Graceful degradation: if `jq` isn't on the target the hook
falls back to **blocking** the call with a redirect message (enforcement still holds), and if the
wrapper failed to stage at all the hook is simply absent and behaviour falls back to the priming
instruction.

### The audio picker: output and input

The top-bar audio button opens a picker with **two sections you set independently** — **Output**
(where Claude's voice plays) and **Input** (which mic listens). Making both explicit means the app
never has to *guess* the capture setup from the output alone: your two picks fully determine the
route and echo-cancellation with no ambiguity. Picking an item doesn't close the menu, so you can
set both in one visit.

- **Output** — **Earpiece**, **Speaker**, **Headset** (only while a headset is connected), and
  **Mute** (suppresses the voice entirely). Headset plays at full-quality media (A2DP).
- **Input** — **Device** (the phone's own built-in mic) and **Headset** (a paired Bluetooth
  headset's own mic; only while one is connected).

Why call-mode matters: hands-free listening normally runs as **communication audio** (like a call)
with the platform echo canceller on, so you can barge in over the phone's speaker. The side effect
is that Android **ducks other apps** — a movie drops to a whisper and the far-field gain clamps a
voice a couple of feet away. The two picks steer around that automatically:

- **Device mic + Earpiece/Speaker** — call-mode capture with the echo canceller, so barge-in works
  over the speaker.
- **Device mic + Headset output** — plain **media mode**: full-quality A2DP in your ears, the phone
  mic with no echo canceller and **no** ducking or gain clamp. It's the **preferred default** the
  moment a headset connects, so you get clean playback plus a clean far-field mic automatically. You
  still have to be near the phone to be heard.
- **Headset mic** — forces the Bluetooth **hands-free profile** so the headset's own mic picks you
  up from across the room. This is call-mode audio by nature, so the headset drops to call quality
  and other apps duck while it's listening (the SCO link also carries playback, so it takes over the
  output). If the hands-free link **fails to engage** — some earbuds refuse it on demand and the
  phone reverts to the mic-less music link — the app detects the dead link within a couple of
  seconds and **falls back to the built-in mic** so you're never left unheard (the mic status line
  says so). Re-selecting **Headset** retries it.

Whatever you choose, capture **restarts live** to match — switching output or input while listening
re-resolves the mic, so it can't get stranded in the wrong mode. If a headset disconnects, the
picker drops its entries and any headset selection falls back (Output → Earpiece, Input → Device).

### Choosing the AI backend and its model

The server drives more than one headless AI. Each **backend** is an entry in an AI registry that
declares how to invoke it and how to read its output, so they share one interface; four ship today:

- **Claude Code** (the default) — `claude` headless in stream-json mode.
- **Codex** (OpenAI's CLI) — `codex exec`; the server captures Codex's own session id and resumes
  it turn to turn. Needs `codex` installed and logged in (`codex login`); host turns run over SSH, so
  set `SPAWNER_SSH_CODEX_BIN` if `codex` isn't on the host's `PATH` (and `SPAWNER_SANDBOX_CODEX_BIN`
  for the sandbox target, analogous to the per-target Claude binaries).
- **Ollama** (through opencode) — `opencode run --format json`; like Codex it captures
  opencode's own `ses_…` session id and resumes it turn to turn. Its models are the `ollama/*`
  catalogue, so **runs stay entirely on-box** against local weights — no cloud round-trip. Needs
  `opencode` installed with an **Ollama provider** in `~/.config/opencode/opencode.jsonc` (an
  `@ai-sdk/openai-compatible` provider whose `baseURL` points at the running Ollama server, e.g.
  `http://localhost:11434/v1`, listing the local models). Set `SPAWNER_SSH_OPENCODE_BIN` if
  `opencode` isn't on the host's `PATH` (and `SPAWNER_SANDBOX_OPENCODE_BIN` for the sandbox). opencode
  keeps sessions in a SQLite DB, so reattach replays history via `opencode export` (and delete uses
  `opencode session delete`) rather than reading files.
  - **Models are discovered automatically.** The server asks opencode which models it can run
    (`opencode models ollama`) at startup and, throttled, whenever an app connects — so the model
    picker always reflects opencode's real catalogue with **no server rebuild and no app update**.
    Adding a model is the usual two local steps, both yours (the server treats opencode as the source
    of truth for what's runnable): `ollama pull <model>`, then add it under the `ollama` provider's
    `models` in `opencode.jsonc`. It then shows up on your next connect. A model you pulled but didn't
    wire into opencode stays hidden — opencode couldn't run it anyway. If the discovery probe ever
    fails (opencode unreachable), the picker falls back to a built-in `qwen2.5-coder` / `llama3.1`
    pair so it's never empty.
- **Zen** (OpenCode Zen subscription through opencode) — also runs through `opencode run --format
  json`, but uses OpenCode Zen's `opencode/*` model catalogue instead of local Ollama. Connect Zen in
  opencode first, then the server discovers it with `opencode models opencode`; if discovery fails,
  the picker keeps a small built-in Zen fallback list.
- **Antigravity** (Google's Gemini-powered `agy` CLI) — `agy --prompt` in its non-interactive
  "print" mode, with `--output-format stream-json` for the machine-readable event stream (a real
  flag, just missing from `agy --help`). It offers the Gemini 3.x models (Pro and Flash, plus hosted
  Claude/GPT-OSS options); agy mints its own conversation id, which it announces at the top of the
  stream, so the server lets the first turn create one, adopts it, and resumes it with
  `--conversation` on every later turn. You get live tool breadcrumbs, per-message replies,
  per-turn token counts, and history replay on reattach.
  Needs `agy` installed and signed in on the host (host turns run over SSH, so set `SPAWNER_SSH_AGY_BIN`
  if `agy` isn't on the host's `PATH`, and `SPAWNER_SANDBOX_AGY_BIN` for the sandbox). **Caveat:** agy
  reports cache *reads* but no cache-write count, and exposes no context-window size — so an agy
  session shows token counts per turn but no cache-warm indicator and no context-remaining badge.

Pick the backend when you spawn — by **voice**, "hey buddy, spawn a codex session", "…on
ollama", or "…on zen" creates that backend's session; a plain spawn uses Claude. The older spoken
"opencode" selector still maps to Ollama. In the **visual New-session picker** (the app or
the browser client), a backend chip row (shown when more than one backend is available) and a model
chip row let you choose both before starting. The new session is stamped with that backend and its
default model.

A session records which backend it runs and which **model**. Each backend has a **default model**
the spawner picks for you, plus a short catalogue you can switch between by voice:

- **"hey buddy, list models"** — speaks the attached session's backend catalogue, numbered, marking
  the current one (Claude: `opus` / `sonnet` / `fable`; Codex on a ChatGPT-account plan: `gpt-5.5`
  and its low/high reasoning presets — the account decides which model ids are selectable; Ollama
  and Zen: whatever opencode is configured to run, discovered live and named by model id).
- **"hey buddy, use model 2"** — switches to that numbered model (say the number — "two" or "2").
  Selecting by **number** is deliberate: it sidesteps having to pronounce awkward model names. The
  choice is durable on the session and takes effect on your next message.

Each session's backend and model are also shown on screen: the sessions drawer tags every row with a
small **"Backend · model"** badge (the backend name is dropped for the default Claude, so a
single-backend setup just shows the model), and the title bar shows the attached session's badge next
to the context meter.

### Token & usage displays

All screen-only (nothing spoken), so hands-free dictation is unaffected. The numbers come straight
from the headless `result` usage — no estimation. See [`docs/protocol.md`](./docs/protocol.md).

- **Token badge** under each reply (toggle in Settings → Appearance): the turn's context and output
  tokens (`24k↑ 340↓`), a **⚡** when it reused a warm prompt cache, and a detailed mode that splits
  fresh vs. cached input.
- **Warm-cache countdown** (toggle in Settings → Appearance) — counts down the ~5-minute window in
  which your next turn reuses the warm prompt cache rather than rebuilding the whole context. This is
  display-only; it's distinct from the Server page's **Warm compress**, which actually triggers a
  compaction near that edge.
- **Title bar** shows the attached session's current context size (`🧠 24k`).
- **Session limit** at the bottom of the sessions drawer — which Claude usage window (rolling 5-hour
  or weekly) is binding and when it resets, from the CLI's `rate_limit_event` (refreshes each turn).
- **📊 Check usage** (drawer button, or "hey buddy, usage") runs `claude -p "/usage"` for the exact
  session/weekly percentages the desktop TUI's `/usage` shows; the voice form also speaks a one-line
  summary.

Each live message also carries a small date/time badge.

## Security

The server can run arbitrary commands (Claude runs with permissions bypassed). **Do not expose it to
the internet without authentication and TLS.** Use a private network / Tailscale, require an auth
token from the app, and constrain spawn directories.

### Transport TLS and mutual TLS (optional)

**In the common deployment, TLS is terminated at a reverse proxy (Caddy) in front of the server:**
the proxy serves `wss://` with a publicly-trusted cert and forwards plain `ws://` to the spawner on
localhost. The app just points at the proxy's `wss://…` URL and authenticates with the token — there
is **no client certificate to install in the app** (removed; if you need mutual TLS, enforce it at
the proxy). By default, with no proxy, the WebSocket is plain `ws://`, which is fine when the only
hop is a Tailscale/WireGuard tunnel (it already encrypts).

If the proxy's `wss://` cert is signed by a **private** CA rather than a public one (e.g. Caddy `tls
internal`), the Android app can trust it via **Settings → Server → Trusted CA** — see "Trusting a
private CA" under the browser-client section for how to import or `adb push` the CA.

The server can also do TLS itself (for setups without a proxy) via these env vars:

- **Server TLS (`wss://`)** — set `SPAWNER_TLS_CERT` and `SPAWNER_TLS_KEY` to a PEM cert/key pair
  (both or neither; one alone is a startup error). The listener then serves `wss://`; point the app
  at a `wss://…` URL.
- **Mutual TLS** — also set `SPAWNER_TLS_CLIENT_CA` to a PEM bundle of the CA(s) that sign your
  client certificates. The server then demands a valid client cert **in addition to** the token, so
  a leaked token alone can't attach (requires the server cert/key pair). The app itself no longer
  presents a client cert, so this path is for non-app clients or is better handled at the proxy.

## Where sessions run: host vs. sandbox

Each session picks an **execution target** at spawn time, a durable per-session choice:

- **host** (default) — turns run as a child process on the host, editing real host files with your
  host toolchain. No configuration needed.
- **sandbox** — turns run inside an isolated container (root *inside* the container) via a
  **rootless** runtime (Podman by default), so no host root is needed. The container is
  **persistent for the session's lifetime** — packages you install and services you start survive
  between turns — and is destroyed when you delete the session. Set `SPAWNER_SANDBOX_IMAGE` to an
  image carrying `claude` + your toolchain to enable it; the voice spawn dialog then adds a "host or
  sandbox?" step, and the visual sidebar's new-session screen shows a **host/sandbox toggle** (host
  by default) so you can pick the target when starting a project there too. The working directory is bind-mounted into the **sandbox container**
  at the same path so edits land there, and the host user's `$HOME` is bind-mounted **read-write at
  the same path** by default so your dotfiles, `~/.claude`, `~/.codex`, and checkouts are available
  and writable in the container just like on the host. (This home mount is the **sandbox
  container's** — the spawner-server container itself mounts no host home; it reads everything over
  SSH.) Tune with the other `SPAWNER_SANDBOX_*` vars. A ready-to-build Arch image and the rootless-Podman
  config live in [`sandbox/`](./sandbox/README.md). Because the server is containerized and
  SSH-native, the container has no runtime of its own, so it drives rootless Podman
  **on the host over SSH** (the same connection host turns use) — set the `SPAWNER_SANDBOX_*` vars in
  the container env as host paths, keep `HOME` pointed at the host user's home, and sandbox sessions
  run on the host alongside host turns.

### The live deployment: a containerized, SSH-native server

The **server runs in a Docker container** that builds the Go binary from source — this is the one
supported deployment. It runs as your ordinary user (never root) and drives the host over **SSH**
(unconditional): `claude` for host sessions and rootless Podman for sandbox sessions both execute
**on the host**, over the same SSH connection, so the container needs no host root and no separate
broker. A session may spawn anywhere on the host (no spawn-directory jail). Transcription is a second container — a resident
WhisperX HTTP server on `:8572`, which lives in the separate `/data/speech_services` stack (it
speaks the same `/inference` contract as whisper.cpp but with accurate, stable word timestamps).
One model handles both dictation and the live hands-free draft; on fast enough hardware
there's no need to split the load. An optional second **fast** draft/detection model can offload
the live draft — the `/data/speech_services` stack also carries a whisper.cpp server on `:8571`;
set `SPAWNER_WHISPER_FAST_URL=http://localhost:8571` to enable the two-model draft/commit split. With
it unset, the **quick** field simply reads "none" and everything routes to the one model. The model(s) are
server-global and can be hot-swapped from **Settings → Audio → Transcription models** (they load
for every device at once): the **full** field is the accurate server (dictation), the **quick**
field the fast one (live hands-free draft + end-token detection). When `SPAWNER_WHISPER_MODELS_DIR`
points at the host's ggml model directory, each field is a dropdown of the **curated English-model
catalogue** — `tiny.en`, `base.en`, `small.en`, `medium.en`, `large-v3-turbo`, `large-v3` (plus any
extra ggml file you dropped in). A model that isn't on disk yet is marked with a **⤓**; applying it
makes the **server download it on demand** from Hugging Face into `SPAWNER_WHISPER_MODELS_DIR`, shows
a live progress bar in the picker, and then hot-loads it — so you never have to fetch model files by
hand, and a **fresh deploy with an empty models dir auto-downloads the boot model** on first start.
Without the dir set, each field falls back to a free-text ggml model name. Both choices are
**persisted to `settings.json`** next to the session state, so a restart or rebuild keeps them
instead of reverting to `SPAWNER_WHISPER_MODEL_NAME` / `SPAWNER_WHISPER_FAST_MODEL_NAME`. Applying
a field's unchanged value is a deliberate **pin**: no reload happens, but a model that so far only
came from the env default gets written to `settings.json`.
(Settings the app owns — the per-device voice prefs — ride along in each `hello` and don't need
server-side storage.)

Bring-up lives in [`deploy/`](./deploy/README.md): fill in the env file's token and run a single
`docker compose up -d --build` from the repo root — the root [`docker-compose.yml`](./docker-compose.yml)
holds **both** the `spawner-server` gateway and the `whisper` transcription server, so one command
builds the binary and launches the whole backend. The server comes up **bare**: it mints its own SSH
keypair on first boot and auto-trusts the loopback host key, so there's nothing to seed by hand. The
one manual step is enabling host access — add the server's generated public key
(`deploy/state/ssh/id_ed25519.pub`, also logged at startup) to the host user's `~/.ssh/authorized_keys`
so the container can SSH in for host turns and the restart button. The app's **restart** button fires
`SPAWNER_RESTART_CMD`, which the server runs on the host over that same Go-native SSH connection (no
openssh client) — launching [`deploy/rebuild-container.sh`](./deploy/rebuild-container.sh) detached, a one-tap
`compose build --no-cache` + recreate that rebuilds the image from current source and recreates the
gateway. The button has a **Rebuild from source** checkbox (default on): leave it on to recompile and
pick up server changes, or clear it for a fast *bounce* that relaunches from the current build without
recompiling. Full design in [`docs/architecture.md`](./docs/architecture.md).

Because that script is launched **detached**, the SSH call returns immediately and says nothing about
whether the build worked — so the script appends its progress to a status file on the host
(`SPAWNER_REBUILD_STATUS_FILE`, default `/tmp/spawner-rebuild.status`) and the server polls it and
pushes each phase to every connected client as a `restart_status` message (`started`, then `finished`
or `failed` with the reason). A finished **build** also sets a *pending bounce* bit — a new image is
staged but the running container isn't on it yet — which the app also receives in `hello_ok` when it
reconnects, so it can keep offering the one-tap bounce. The server speaks a line when a build is
ready or a rebuild fails.

The **sessions toolbar** (top of the drawer, right of the "Sessions" title) shows all of this at a
glance: a small **green dot** while the WebSocket is connected and a **red** one when it isn't, a
**spinner** while a build or rebuild is running, and an **update glyph** once a build has finished
but the container hasn't been bounced onto the new image yet. The spinner and glyph clear themselves
when the bounce lands, and the pending glyph comes back after a reconnect (from `hello_ok`) so it
survives the very restart it's reporting on. A plain *bounce* is fast enough that it shows no
spinner. Every indicator carries a spoken content description for screen readers.

## Building & running from source (local dev)

The supported **deployment** is the container above. For quick local iteration you can also build
the single binary and run it directly:

```bash
# build the server (the Go module is under server/)
go build -C server -o ~/.local/bin/spawner-server .

# run it on :8080; add SPAWNER_WHISPER_URL/_FAST_URL for voice
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8080 \
  ~/.local/bin/spawner-server

# drive it with the text client (spawn, then dictate to Claude Code)
go run -C server ./cmd/wsclient -url ws://localhost:8080/ws
#   hey buddy spawn a new session → say a full path like /home/you/git/demo → yes → then dictate
```

- `claude` authenticates via your host creds in `~/.claude` + `~/.claude.json` (or set
  `ANTHROPIC_API_KEY`). Sessions can spawn anywhere on the target host (no directory jail).
- Voice end-to-end needs the resident whisper server running and `SPAWNER_WHISPER_URL` pointed at
  it. The whisper (WhisperX on `:8572`, whisper.cpp on `:8571`) and Kokoro TTS (`:8880`) containers
  live in the separate **`/data/speech_services`** stack (`docker compose up -d` there) — bring that
  up alongside the gateway.
- To test a change without killing a live turn, run the fresh binary on a scratch port
  (`SPAWNER_ADDR=:8557`) with a separate `SPAWNER_STATE` — see [`deploy/README.md`](./deploy/README.md).

### The browser client (Compose Multiplatform)

The same UI as the Android app also runs **in a browser** via Kotlin/Wasm — one shared `commonMain`
renders identical composables on both. Build the web bundle and let the server host it:

```bash
# build the web bundle (index.html + spawnerweb.js + .wasm) — JDK 17+ on JAVA_HOME,
# no Android SDK needed (first build downloads Gradle/Node/Binaryen)
./android/gradlew -p android :app:wasmJsBrowserDistribution
#   output: android/app/build/dist/wasmJs/productionExecutable/

# point the server at it — served at "/" alongside the "/ws" gateway (one binary)
SPAWNER_TOKEN=devsecret SPAWNER_ADDR=:8080 \
  SPAWNER_WEB_DIR=android/app/build/dist/wasmJs/productionExecutable \
  ~/.local/bin/spawner-server
#   then open http://<host>:8080/ in a browser (needs a Wasm-GC browser — recent Firefox/Chrome)
```

In the **containerized deploy** the bundle isn't mounted — it's **baked into the image** at
`/srv/web` (with `SPAWNER_WEB_DIR=/srv/web`). `deploy/rebuild-container.sh` stages the Gradle output
into the image build context, so a `rebuild` press of the restart button ships the current client;
a `bounce` won't. Rebuild the bundle out-of-band (the `:app:wasmJsBrowserDistribution` task above)
whenever the UI changes, then rebuild the container to publish it.

The bundle defaults its WebSocket to the **same origin** it was served from (`/ws`, `wss://` when the
page is https), so a server-hosted client connects with no setup — you only edit the URL/token under
**Settings → Server** if you're pointing elsewhere. The static assets are public; the privileged
surface stays behind the token-authenticated `/ws` handshake (and mutual TLS if configured).

**Server URL — a bare host is enough, and the port picks the scheme.** The **Settings → Server** URL
field accepts just a hostname: the client fills in the scheme and gateway path for you. Whether a
**port** is given decides the scheme, matching the usual deployment — a bare host means "go through
the TLS reverse proxy," and an explicit port means "talk straight to that port":

- `cs.bam` → `wss://cs.bam/ws` (secure, port 443 — through the proxy);
- `cs.bam:8098` → `ws://cs.bam:8098/ws` (plain, straight to the gateway port).

A pasted `http(s)://` URL is mapped (`http`→`ws`, `https`→`wss`) and a fully-formed `ws(s)://host/ws`
is left untouched. So a Caddy site (e.g. `cs.bam` reverse-proxying to the gateway) transparently
carries both the web client at `/` and the `/ws` WebSocket upgrade over `wss://`, while the bare port
form stays available for a direct, no-TLS connection.

**Trusting a private CA (Android).** If the proxy serves `wss://` with a **private** certificate — a
Caddy `tls internal` site, whose cert isn't in the device trust store — the Android app won't
validate it by default. **Settings → Server → Trusted CA** imports a CA (a `.crt`/PEM file) that the
app trusts *in addition to* the system store, so that server validates while public certs still work.
Get the CA from the caddyedit editor's **Download CA** button. A CA `adb push`ed into the app's
external files dir (`Android/data/<pkg>/files/caddy-root.crt`) is auto-imported on the next connect,
for hands-off setup. (The browser client has no such control — trust the CA in the browser/OS
instead.)

Text chat, the session drawer, hosts/identities, usage, **file transfer** (the 📎 button — the same
upload/download flow as the app, reading/writing the browser's own files), and **spawning new
sessions** (the same New-session picker as the app — target/host + backend/model + filesystem browse,
sharing one `commonMain` `BrowseScreen`) all work. Because a mouse can't obviously "swipe", the
browser client also shows **visible controls** for the touch gestures: a chevron handle above the
message box opens the command tray, a **Refresh** button sits beside **New** in the sessions drawer,
and **Shift+Enter sends** a message (plain Enter is a newline) — the same chord works from a
Bluetooth keyboard paired to the Android app.

**Voice works in the browser too**: hold the mic button to talk — the client captures the microphone
via the Web Audio API, downsamples it to 16 kHz mono PCM16, and streams the clip to the server's
Whisper over the same socket (the `pcm16` codec — no Opus/ffmpeg needed), exactly like the phone's
push-to-talk. Replies are **read aloud** — with the server's Kokoro voice when it offers TTS (see
"Server voice" above), else the browser's built-in `SpeechSynthesis` — and the stop button (or the
"stop" barge-in) halts playback. The mic needs a **secure context** (https or localhost) and
microphone permission.

**Hands-free (always-listening) works in the browser too**: swipe the mic button up to switch it on
and the client keeps the mic open, running a Web-Audio voice-activity detector that mirrors the
phone's — it starts an utterance after a moment of sustained speech and ends it on a pause (tuned by
the same **Audio → threshold / VAD** dials the phone uses), then ships each utterance the same way a
push-to-talk clip goes, so the server accumulates your speech until the **end token** ("beep")
commits it. It rejects its own text-to-speech from re-triggering the mic while it's speaking. Because
the browser needs a user gesture to open the mic, hands-free is a **per-session** toggle (it isn't
restored automatically on load). The browser speaks to the OS default output sink and can't route
between devices, so the audio-output button offers the two states that matter: **Speaker** (voice
on) or **Mute** (voice off, which also stops any reply already being spoken); the choice is saved.

By default the sessions sidebar is a **modal drawer at every width** — it never opens on a swipe.
The **☰ button in the top bar is the only way to show it**, at any window size; tapping outside it,
swiping it left, or pressing back closes it again. The chat therefore keeps the whole window until
you ask for the session list. (The right-to-left swipe on the chat still swaps sessions — that
gesture is unchanged.)

**Pinning the sidebar.** A **pin button** sits at the top-right of the sidebar, next to the
"Sessions" title. Tap it and the sidebar becomes a **permanent docked rail**: it stays on screen and
the chat column shifts over beside it, so the full message text stays visible instead of being
covered by an overlay. The ☰ button disappears while pinned (the list is already there). Tap the pin
again to unpin and go back to the overlay drawer. The choice is **persisted** (Android
`SharedPreferences` / browser `localStorage`) and applies to both clients — it's most useful on a
wide browser window or tablet, but works at any size.

> **Secure context required.** The client only connects from a **secure context** — https, or
> `localhost`/`127.0.0.1`. Served over plain http from a real hostname the browser marks the origin
> insecure and the connection fails, so put the server behind TLS (a `wss://` cert, or a reverse proxy
> like Caddy) for anything but local testing.

Working **on** the web client (source-set layout, the Kotlin↔JS interop idiom, the build/iterate
loop) is documented in `docs/web-client.md`.

## Project history

Built in phases: the response-capture decision and spec (Phase 0), the Go server (Phase 1),
transcription and dialog (Phase 2), the Kotlin/Compose app (Phase 3), passthrough/attach (Phase 4),
and polish (Phase 5 — auto-reconnect, barge-in, abort-a-turn, notifications, and the token/usage
displays above). All phases are complete and verified live. Active work and any remaining open items
live in the single task tracker, [`TODO.toml`](./TODO.toml).
</content>
</invoke>

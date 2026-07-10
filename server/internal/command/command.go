// Package command turns a transcribed utterance into an intent. Matching is
// keyword/intent based (tolerant of filler words), per docs/commands.md. The
// authoritative grammar lives in that doc; keep this in sync with it.
package command

import (
	"sort"
	"strconv"
	"strings"
)

// Kind enumerates recognized control intents.
type Kind string

const (
	Spawn      Kind = "spawn"
	Attach     Kind = "attach"
	Detach     Kind = "detach"
	List       Kind = "list"
	Kill       Kind = "kill"
	Status     Kind = "status"
	Cancel     Kind = "cancel"
	Stop       Kind = "stop"        // stop speaking (barge-in)
	AbortTurn  Kind = "abort_turn"  // cancel the running Claude turn
	Help       Kind = "help"        // list available commands
	ReadLast   Kind = "read_last"   // re-read the last N Claude replies aloud
	Clear      Kind = "clear"       // rotate the session's Claude context (keep history for display)
	Compress   Kind = "compress"    // summarize the context, then rotate — carry a condensed summary forward
	Usage      Kind = "usage"       // report the Claude plan's usage (session/week % left), via `/usage`
	Rename     Kind = "rename"      // rename the currently-attached session
	ListModels Kind = "list_models" // list the attached session's backend's models
	UseModel   Kind = "use_model"   // switch the attached session's model by number
	Unknown    Kind = "unknown"
)

// Intent is a parsed control command. Arg holds a session name for attach/kill.
// For Spawn, Location holds the spoken path after "in"/"at" (e.g. "git personal")
// and New is true when the user asked for a new project (create) rather than to
// browse existing ones.
type Intent struct {
	Kind     Kind
	Arg      string
	Location string
	New      bool
	Count    int    // for ReadLast: how many recent replies to re-read; for UseModel: the 1-based model number
	Agent    string // for Spawn: the AI backend chosen inline ("codex"); empty = default backend
}

// wakePhrases is the single source of truth for the wake token — the spoken
// prefix that flags an utterance as a control command rather than dictation. It
// is the wake-word analogue of command.Registry's per-command Aliases: the
// canonical "hey buddy" first, then accepted whisper mishearings. TWO-word forms
// cover mishearings of "buddy"; SINGLE-word forms cover cases where whisper
// collapsed the whole phrase into one token (e.g. "everybody" for "hey buddy").
// Teach the server a new mishearing by adding it here — every wake match reads
// from this list, nothing is hardcoded elsewhere.
//
// Caution on single-word aliases: they are ordinary English words, so they wake
// more eagerly than the distinctive two-word "hey buddy" (e.g. "everybody knows"
// now strips a wake). They earn their place only because whisper reliably emits
// them for the real wake word; keep the single-word set small.
var wakePhrases = [][]string{
	{"hey", "buddy"},
	{"hey", "bud"},
	{"hey", "body"},
	{"hey", "buddie"},
	{"hey", "budy"},
	{"everybody"},
	{"heybuddy"},
}

// commandVocab is the distinctive control-command vocabulary — the verbs and
// nouns whisper should lean toward when transcribing a "hey buddy" command. It
// feeds the STT initial-prompt bias (see gateway.vocabBias) so command words
// survive transcription instead of being mangled into similar-sounding words.
// It is NOT the parser's matching table (Parse owns that, tolerant of many
// phrasings); it is the salient subset worth naming to whisper. Add a command's
// key word here when you add the command.
var commandVocab = []string{
	"spawn", "attach", "detach", "list", "kill", "status", "cancel",
	"stop", "abort", "help", "read last", "clear", "compress", "compact",
	"usage", "rename", "session", "project", "model", "models", "codex",
}

// Vocabulary returns the control words worth biasing STT toward: the canonical
// wake phrase (wakePhrases[0]) followed by commandVocab. gateway.vocabBias folds
// this into the whisper initial-prompt so the wake word and command verbs
// transcribe reliably. This is the single source of truth for that bias list —
// nothing hardcodes the command words elsewhere.
func Vocabulary() []string {
	out := make([]string, 0, len(commandVocab)+1)
	out = append(out, strings.Join(wakePhrases[0], " "))
	return append(out, commandVocab...)
}

// StripWake removes an optional leading wake phrase (any wakePhrases entry, with
// optional punctuation) and reports whether it was present. Used to distinguish
// control commands from plain dictation while attached.
func StripWake(text string) (rest string, hadWake bool) {
	words := strings.Fields(strings.TrimSpace(text))
	if n := wakeAt(words, 0); n > 0 {
		return strings.Join(words[n:], " "), true
	}
	return strings.TrimSpace(text), false
}

// SplitWake locates the wake phrase in the text and splits into dictation +
// command. If the wake phrase appears MULTIPLE times (the user self-corrected the
// command), the LAST one wins: before = text preceding the FIRST wake (the
// dictation), after = text following the LAST wake (the command); anything in
// between (earlier command attempts) is discarded.
func SplitWake(text string) (before, after string, found bool) {
	words := strings.Fields(strings.TrimSpace(text))
	first, last, lastN := -1, -1, 0
	for i := 0; i < len(words); {
		if n := wakeAt(words, i); n > 0 {
			if first < 0 {
				first = i
			}
			last, lastN = i, n
			i += n // skip the matched phrase so it isn't re-scanned
			continue
		}
		i++
	}
	if first < 0 {
		return strings.TrimSpace(text), "", false
	}
	return strings.Join(words[:first], " "), strings.Join(words[last+lastN:], " "), true
}

// wakeAt reports how many words the wake phrase at words[i] consumes (0 if none),
// tolerating surrounding punctuation and case. The longest matching phrase wins,
// so a one-word alias can't shadow the canonical two-word form.
func wakeAt(words []string, i int) (consumed int) {
	for _, phrase := range wakePhrases {
		if len(phrase) > consumed && phraseAt(words, i, phrase) {
			consumed = len(phrase)
		}
	}
	return consumed
}

// phraseAt reports whether phrase matches the words starting at i, comparing
// lowercased and punctuation-trimmed.
func phraseAt(words []string, i int, phrase []string) bool {
	if i+len(phrase) > len(words) {
		return false
	}
	for k, p := range phrase {
		if strings.Trim(strings.ToLower(words[i+k]), ",.!?") != p {
			return false
		}
	}
	return true
}

// listQualifiers are the words that may follow "list" for the List command
// (so "list sessions"/"list all" match but "list the files ..." dictates).
var listQualifiers = map[string]bool{
	"sessions": true, "session": true, "projects": true, "all": true,
	"recent": true, "alphabetical": true, "them": true, "everything": true,
}

// normalize lowercases and turns sentence punctuation into word breaks, so
// whisper's capitalization and trailing "." / "," don't defeat token matching
// (e.g. "List sessions." → "list sessions").
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch r {
		case ',', '.', '!', '?', ';', ':', '"':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// Parse classifies an utterance (already wake-stripped) into an intent.
//
// Matching is deliberately precise, not loose substring containment: a control
// command is a SHORT utterance led by a command verb (or a distinctive phrase),
// so dictation like "list the files in this module" or "spawn a goroutine" is
// NOT misread as a command and instead flows through to Claude. Stop/Cancel stay
// broad because barge-in / dialog-abort must fire in any context.
func Parse(text string) Intent {
	t := normalize(text)
	if t == "" {
		return Intent{Kind: Unknown}
	}
	words := strings.Fields(t)
	first := words[0]
	n := len(words)
	word2 := ""
	if n > 1 {
		word2 = words[1]
	}

	switch {
	// Barge-in / dialog abort: always eligible, regardless of length.
	case t == "stop" || t == "quiet" || t == "hush" || t == "enough",
		contains(t, "stop talking", "stop speaking", "stop reading", "be quiet", "shut up"):
		return Intent{Kind: Stop}
	// Abort the running turn (kill the claude child). Checked before Cancel/Kill so
	// "cancel the turn" / "kill the turn" abort the turn, not the message/session.
	case t == "abort" || first == "abort",
		contains(t, "stop the turn", "stop the command", "stop the job", "stop the task",
			"stop working", "cancel the turn", "cancel the command", "kill the turn",
			"kill the command", "abort the turn", "halt the turn"):
		return Intent{Kind: AbortTurn}
	case t == "cancel" || first == "cancel" ||
		contains(t, "cancel message", "cancel that", "never mind", "nevermind", "forget it", "scrap that", "scrap it"):
		return Intent{Kind: Cancel}

	// Read last: "read last", "read last 3", "read the last two", "read that
	// back", "say that again", "repeat that/last".
	case first == "read" && contains(t, "read last", "read the last", "read that", "read it", "read again"),
		contains(t, "say that again", "say it again", "repeat that", "repeat last", "read that back", "read it back"):
		return Intent{Kind: ReadLast, Count: readCount(words)}

	case (first == "help" && n <= 2) || first == "commands" ||
		contains(t, "what can you do", "what can i say", "list commands", "show commands", "available commands", "which commands"):
		return Intent{Kind: Help}

	// Models: list the attached session's backend models, or switch to one by
	// NUMBER ("use model 3"). Ordinal selection deliberately sidesteps
	// hard-to-say model names (e.g. "gpt-5.5" reasoning presets). Checked before
	// List/Status so "list models" isn't swallowed by the bare-"list" case.
	case leadsWith(t, "list models", "list the models", "show models", "show the models", "list available models"),
		contains(t, "what models", "which models", "what are the models"):
		return Intent{Kind: ListModels}
	case first == "use" && contains(t, "model"),
		leadsWith(t, "switch to model", "switch model", "select model", "set model", "change model", "change to model", "pick model"):
		return Intent{Kind: UseModel, Count: modelIndex(words)}

	// Spawn: "spawn … session/project", or a leading new-session/project phrase.
	case first == "spawn" && contains(t, "session", "project"),
		leadsWith(t, "new session", "new project", "create a session", "create a project", "start a session", "start a project"):
		// Pull an inline backend choice ("spawn a codex session", "… on codex") out
		// first, so its word doesn't leak into the parsed location.
		agentID, rest := extractSpawnAgent(t)
		// Prefer an explicit preposition ("spawn a session in git personal"); if
		// there's none, take whatever path was spoken right after "session"/
		// "project" ("spawn a new session bam git personal") so a one-shot command
		// with an inline location still jumps straight there instead of dropping it.
		loc := afterAny(rest, "in", "at", "under", "inside")
		if loc == "" {
			loc = afterAny(rest, "session", "project")
		}
		return Intent{
			Kind: Spawn,
			// Detect on the backend-stripped text so "new codex project" still reads
			// as new-project (the backend word sits between "new" and "project").
			New:      contains(rest, "new project", "new repo", "new folder", "create a project", "start a project"),
			Location: loc,
			Agent:    agentID,
		}

	// Detach: bare "detach"/"detach now", or an explicit phrase.
	case first == "detach" && n <= 2, contains(t, "stop dictating", "stop listening"):
		return Intent{Kind: Detach}

	// Kill: short "kill <name>", or an explicit "… session" phrase.
	case first == "kill" && n <= 3, contains(t, "kill session", "stop session", "end session", "close session"):
		return Intent{Kind: Kill, Arg: argAfter(t, "session", "kill")}

	// Attach: "attach to <name>" — capture everything after "to" (multi-word dir
	// names) and let the server fuzzy-match it.
	case first == "attach" && word2 == "to" && n <= 8, contains(t, "attach to"):
		arg := afterAny(t, "to")
		if arg == "" {
			arg = afterAny(t, "attach")
		}
		return Intent{Kind: Attach, Arg: arg}

	// List: "list" optionally followed by a session qualifier.
	case first == "list" && (n == 1 || listQualifiers[word2]),
		contains(t, "what sessions", "which sessions", "sessions are"):
		return Intent{Kind: List}

	// Status: bare "status", or an explicit phrase.
	case first == "status" && n <= 2, contains(t, "what's it doing", "whats it doing", "what is it doing"):
		return Intent{Kind: Status}

	// Clear: rotate Claude's context to a fresh session_id. History is KEPT on
	// disk for display — Claude just stops re-reading it. Deliberately does NOT
	// match "clear history" (that would imply deletion, which this never does).
	case first == "clear" && (n == 1 || word2 == "context" || word2 == "session"),
		contains(t, "clear the context", "clear the session", "reset context",
			"reset the context", "fresh context", "start fresh", "wipe context"):
		return Intent{Kind: Clear}

	// Compress: summarize Claude's context, then rotate to a fresh session_id
	// seeded with that summary — the /compact analogue of clear (context is
	// condensed, not dropped). "compact"/"compress"/"condense"/"summarize the
	// context" all match; like clear, history is KEPT on disk.
	case first == "compress" && (n == 1 || word2 == "context" || word2 == "session"),
		first == "compact" && (n == 1 || word2 == "context" || word2 == "session"),
		contains(t, "compress the context", "compact the context", "condense the context",
			"condense context", "summarize the context", "summarize context", "compact it"):
		return Intent{Kind: Compress}

	// Usage: report the Claude plan's usage limits (session/week % used, resets).
	// Bare "usage", or phrasings asking how much is left / used.
	case first == "usage" && n <= 2,
		contains(t, "how much usage", "usage left", "usage limit", "how much have i used",
			"how much is left", "check usage", "my usage", "show usage"):
		return Intent{Kind: Usage}

	// Rename: rename the currently-attached session. "rename to <name>", "rename
	// this session <name>", "call this <name>", "name this session <name>". The
	// new name is whatever follows the anchor keyword; the server sanitizes it to
	// a single token, so multi-word is tolerated. Only parsed post-wake (the
	// dispatch path only reaches Parse when a wake word was present), so the
	// broad "call this"/"name this" phrasings can't hijack dictation.
	case first == "rename",
		leadsWith(t, "call this", "name this"),
		contains(t, "rename to", "rename this", "rename it", "rename session", "rename the session"):
		arg := afterAny(t, "to")
		if arg == "" {
			arg = afterAny(t, "session")
		}
		if arg == "" {
			arg = afterAny(t, "this", "it")
		}
		if arg == "" && first == "rename" {
			arg = afterAny(t, "rename")
		}
		return Intent{Kind: Rename, Arg: arg}

	default:
		return Intent{Kind: Unknown}
	}
}

// ApplyAliases rewrites recognized mis-transcriptions to canonical command words
// before parsing (e.g. "attached" -> "attach"). Keys may be multi-word phrases
// ("the tach" -> "detach"); longest phrases match first. Whole-word, case- and
// punctuation-insensitive.
func ApplyAliases(text string, aliases map[string]string) string {
	if len(aliases) == 0 {
		return text
	}
	type alias struct {
		from []string
		to   string
	}
	als := make([]alias, 0, len(aliases))
	for k, v := range aliases {
		if f := strings.Fields(strings.ToLower(k)); len(f) > 0 && v != "" {
			als = append(als, alias{f, v})
		}
	}
	sort.Slice(als, func(i, j int) bool { return len(als[i].from) > len(als[j].from) })

	words := strings.Fields(text)
	out := make([]string, 0, len(words))
	for i := 0; i < len(words); {
		matched := false
		for _, a := range als {
			n := len(a.from)
			if i+n > len(words) {
				continue
			}
			ok := true
			for j, fw := range a.from {
				if strings.Trim(strings.ToLower(words[i+j]), ",.!?;:\"") != fw {
					ok = false
					break
				}
			}
			if ok {
				out = append(out, a.to)
				i += n
				matched = true
				break
			}
		}
		if !matched {
			out = append(out, words[i])
			i++
		}
	}
	return strings.Join(out, " ")
}

// numWords maps small spoken numbers to ints, so "read last three" works
// alongside "read last 3".
var numWords = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5,
	"six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10,
}

// readCount extracts the count for a ReadLast command: the number after "last"
// (digit or word), else 1.
func readCount(words []string) int {
	for i, w := range words {
		if strings.Trim(strings.ToLower(w), ",.!?") == "last" && i+1 < len(words) {
			if n := wordToNum(words[i+1]); n > 0 {
				return n
			}
		}
	}
	return 1
}

// spawnAgentWords maps a spoken backend name to its agent id for inline spawn
// selection. Only distinctive, non-path-like names belong here: "claude" is
// intentionally absent — it's the default backend AND a common path token (dirs
// like claude_spawner), so treating it as a selector would corrupt locations.
var spawnAgentWords = map[string]string{"codex": "codex"}

// extractSpawnAgent pulls an inline backend choice out of a spawn utterance and
// returns the chosen agent id (empty if none) plus the utterance with the
// backend phrase removed, so the caller parses the location cleanly. A backend
// word counts as a selector ONLY in selector position — right before
// "session"/"project" ("codex session") or right after "on"/"using"/"with" ("…
// on codex"); elsewhere it's left in place as an ordinary path token.
func extractSpawnAgent(t string) (agentID, rest string) {
	words := strings.Fields(t)
	drop := make([]bool, len(words))
	for i, w := range words {
		id, ok := spawnAgentWords[w]
		if !ok {
			continue
		}
		prevSel := i > 0 && (words[i-1] == "on" || words[i-1] == "using" || words[i-1] == "with")
		nextNoun := i+1 < len(words) && (words[i+1] == "session" || words[i+1] == "project")
		if !prevSel && !nextNoun {
			continue // a path token like ".../codex-foo", not a backend selector
		}
		agentID = id
		drop[i] = true
		if prevSel {
			drop[i-1] = true
		}
	}
	out := words[:0:0]
	for i, w := range words {
		if !drop[i] {
			out = append(out, w)
		}
	}
	return agentID, strings.Join(out, " ")
}

// modelIndex extracts the 1-based model number from a UseModel command: the
// first number-bearing token anywhere in the utterance (digit or word), so both
// "use model 3" and "use model three" work. 0 if none was spoken (the gateway
// then reminds the user to say a number).
func modelIndex(words []string) int {
	for _, w := range words {
		if n := wordToNum(w); n > 0 {
			return n
		}
	}
	return 0
}

func wordToNum(w string) int {
	w = strings.Trim(strings.ToLower(w), ",.!?")
	if n, err := strconv.Atoi(w); err == nil && n > 0 {
		return n
	}
	return numWords[w]
}

// leadsWith reports whether t is exactly a phrase or begins with one (+ a space).
func leadsWith(t string, phrases ...string) bool {
	for _, p := range phrases {
		if t == p || strings.HasPrefix(t, p+" ") {
			return true
		}
	}
	return false
}

func contains(t string, subs ...string) bool {
	for _, s := range subs {
		if strings.Contains(t, s) {
			return true
		}
	}
	return false
}

// afterAny returns everything following the first occurrence of any keyword
// (as a whole word), e.g. afterAny("spawn a session in git personal", "in") ->
// "git personal". Empty if no keyword is present.
func afterAny(t string, keywords ...string) string {
	words := strings.Fields(t)
	for i, w := range words {
		for _, kw := range keywords {
			if w == kw {
				return strings.Join(words[i+1:], " ")
			}
		}
	}
	return ""
}

// argAfter returns the token following a keyword. Keywords are tried in priority
// order (e.g. prefer the token after "to" over the one after "attach").
func argAfter(t string, keywords ...string) string {
	words := strings.Fields(t)
	for _, kw := range keywords {
		for i, w := range words {
			if w == kw && i+1 < len(words) {
				return words[i+1]
			}
		}
	}
	return ""
}

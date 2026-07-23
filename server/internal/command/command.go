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
	Spawn       Kind = "spawn"
	Attach      Kind = "attach"
	Detach      Kind = "detach"
	Swap        Kind = "swap" // toggle back to the previously attached session
	List        Kind = "list"
	Kill        Kind = "kill"
	Status      Kind = "status"
	Cancel      Kind = "cancel"
	Stop        Kind = "stop"         // stop speaking (barge-in)
	AbortTurn   Kind = "abort_turn"   // cancel the running Claude turn
	Help        Kind = "help"         // list available commands
	ReadLast    Kind = "read_last"    // re-read the last N Claude replies aloud
	Clear       Kind = "clear"        // rotate the session's Claude context (keep history for display)
	Compress    Kind = "compress"     // summarize the context, then rotate — carry a condensed summary forward
	Usage       Kind = "usage"        // report the Claude plan's usage (session/week % left), via `/usage`
	Rename      Kind = "rename"       // rename the currently-attached session
	ListModels  Kind = "list_models"  // list the attached session's backend's models
	UseModel    Kind = "use_model"    // switch the attached session's model by number
	Scratch     Kind = "scratch"      // toggle scratch mode: detached, echo transcriptions back aloud
	SummaryOnly Kind = "summary_only" // speak only the final turn result; intermediate steps beep
	ListJobs    Kind = "list_jobs"    // list the attached session's detached background jobs
	KillJob     Kind = "kill_job"     // kill one of the attached session's background jobs by number
	JobStatus   Kind = "job_status"   // report the attached session's background-job summary
	Restart     Kind = "restart"      // restart/rebuild the spawner server (SPAWNER_RESTART_CMD)
	Unknown     Kind = "unknown"
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
	Profile  string // for Spawn: the execution profile spoken inline ("sandbox"); empty = default profile
	// Name, for Spawn, is the session's spoken name ("called trashbot"); empty
	// means default to the folder basename.
	Name string
}

// wakePhrases is the DEFAULT wake token — the canonical "hey buddy" first, then
// accepted whisper mishearings. TWO-word forms cover mishearings of "buddy";
// SINGLE-word forms cover cases where whisper collapsed the whole phrase into one
// token (e.g. "everybody" for "hey buddy").
//
// It is no longer the runtime source of truth: the wake token (and the end/speak
// tokens) are now the app-managed spoken-token catalogue, which the gateway feeds
// into the matchers below as an explicit phrase set. This list survives only as
// that catalogue's first-run seed (DefaultWakePhrases) and as the fallback the
// bare StripWake/SplitWake helpers use — the with-phrases variants match ONLY the
// set they are handed, so a configured catalogue fully REPLACES these built-ins.
//
// Caution on single-word aliases: they are ordinary English words, so they wake
// more eagerly than the distinctive two-word "hey buddy" (e.g. "everybody knows"
// strips a wake). They earn their place only because whisper reliably emits them
// for the real wake word; keep the single-word set small.
var wakePhrases = [][]string{
	{"hey", "buddy"},
	{"hey", "bud"},
	{"hey", "body"},
	{"hey", "buddie"},
	{"hey", "buddies"},
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
	"stop", "abort", "help", "read last", "replay", "clear", "compress", "compact",
	"usage", "rename", "session", "project", "model", "models", "codex", "opencode",
	"scratch", "summary", "job", "jobs", "restart", "rebuild", "server",
	"called", "named", "profile", "swap",
}

// Vocabulary returns the control words worth biasing STT toward: commandVocab,
// the distinctive command verbs/nouns. gateway.vocabBias folds this into the
// whisper initial-prompt so the command verbs transcribe reliably, and appends
// the connection's live wake/speak phrases (from the spoken-token catalogue) on
// top — so the wake word itself is biased from the configured catalogue, not a
// hardcoded default here.
func Vocabulary() []string {
	return append([]string(nil), commandVocab...)
}

// DefaultWakePhrases returns a copy of the built-in wake phrases, for seeding the
// app-managed spoken-token catalogue on first run (see spoken.DefaultTokens).
func DefaultWakePhrases() [][]string {
	out := make([][]string, len(wakePhrases))
	for i, p := range wakePhrases {
		out[i] = append([]string(nil), p...)
	}
	return out
}

// WakePhrase tokenizes a COMMA-SEPARATED phrase list ("hey buddy, hey bud, ok
// buddy") into the phrase form the matchers below accept (lowercased,
// punctuation-trimmed words), returning nil for a blank token. The spoken-token
// catalogue now supplies wake/end/speak phrases (spoken.Phrases), so this survives
// as a tokenizing helper for tests and any ad-hoc comma-list callers.
func WakePhrase(token string) [][]string {
	var phrases [][]string
	for _, variant := range strings.Split(token, ",") {
		var phrase []string
		for _, w := range strings.Fields(strings.ToLower(variant)) {
			if w = strings.Trim(w, ",.!?"); w != "" {
				phrase = append(phrase, w)
			}
		}
		if len(phrase) > 0 {
			phrases = append(phrases, phrase)
		}
	}
	return phrases
}

// SplitOn splits text on the FIRST occurrence of any phrase in `phrases`: before =
// the words up to the first match, after = the words following it. It backs both
// the dictation gate's speak token (a start marker) and the end token (a commit
// marker) — each a phrase set independent of the wake word. No match ⇒
// found=false, before = the whole text.
func SplitOn(text string, phrases [][]string) (before, after string, found bool) {
	words := strings.Fields(strings.TrimSpace(text))
	for i := 0; i < len(words); i++ {
		if n := phrasesAt(words, i, phrases); n > 0 {
			return strings.Join(words[:i], " "), strings.Join(words[i+n:], " "), true
		}
	}
	return strings.TrimSpace(text), "", false
}

// StripWake removes an optional leading wake phrase (any default wakePhrases
// entry, with optional punctuation) and reports whether it was present. Used to
// distinguish control commands from plain dictation while attached. The gateway
// uses StripWakeWith with the configured catalogue instead; this bare form (on the
// built-in defaults) backs tests and any callerless-of-a-conn path.
func StripWake(text string) (rest string, hadWake bool) { return StripWakeWith(text, wakePhrases) }

// StripWakeWith strips a leading wake phrase drawn from `phrases` — the EXACT set
// to match (the app-managed spoken-token catalogue's wake phrases), replacing the
// built-in defaults rather than adding to them. An empty set matches nothing.
func StripWakeWith(text string, phrases [][]string) (rest string, hadWake bool) {
	words := strings.Fields(strings.TrimSpace(text))
	if n := wakeAt(words, 0, phrases); n > 0 {
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
	return SplitWakeWith(text, wakePhrases)
}

// SplitWakeWith is SplitWake matching the EXACT `phrases` set (the app-managed
// catalogue's wake phrases) instead of the built-in defaults. Empty ⇒ no match.
func SplitWakeWith(text string, phrases [][]string) (before, after string, found bool) {
	words := strings.Fields(strings.TrimSpace(text))
	first, last, lastN := -1, -1, 0
	for i := 0; i < len(words); {
		if n := wakeAt(words, i, phrases); n > 0 {
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

// SplitWakeAll splits the text on EVERY wake occurrence: `before` is the leading
// dictation (text preceding the FIRST wake) and `commands` is the ordered list of
// command segments, one per wake phrase — each the words following that wake up to
// the next wake (or end of text), empties dropped. This lets a user chain several
// "hey buddy <command>" commands in a single utterance and have them run in order
// (whereas SplitWake keeps only the last). No wake ⇒ nil commands, before = text.
func SplitWakeAll(text string) (before string, commands []string) {
	return SplitWakeAllWith(text, wakePhrases)
}

// SplitWakeAllWith is SplitWakeAll matching the EXACT `phrases` set (the
// app-managed catalogue's wake phrases) instead of the built-in defaults.
func SplitWakeAllWith(text string, phrases [][]string) (before string, commands []string) {
	words := strings.Fields(strings.TrimSpace(text))
	type span struct{ start, end int }
	var wakes []span
	for i := 0; i < len(words); {
		if n := wakeAt(words, i, phrases); n > 0 {
			wakes = append(wakes, span{i, i + n})
			i += n // skip the matched phrase so it isn't re-scanned
			continue
		}
		i++
	}
	if len(wakes) == 0 {
		return strings.TrimSpace(text), nil
	}
	before = strings.TrimSpace(strings.Join(words[:wakes[0].start], " "))
	for k, w := range wakes {
		end := len(words)
		if k+1 < len(wakes) {
			end = wakes[k+1].start
		}
		if seg := strings.TrimSpace(strings.Join(words[w.end:end], " ")); seg != "" {
			commands = append(commands, seg)
		}
	}
	return before, commands
}

// wakeAt reports how many words the wake phrase at words[i] consumes (0 if none),
// tolerating surrounding punctuation and case. Only `phrases` is considered (the
// configured catalogue set, or the built-in defaults for the bare helpers) — the
// longest matching phrase wins, so a one-word alias can't shadow a longer form.
func wakeAt(words []string, i int, phrases [][]string) (consumed int) {
	return phrasesAt(words, i, phrases)
}

// phrasesAt reports how many words the longest phrase in `phrases` matching at
// words[i] consumes (0 if none) — the phrase-set primitive shared by wakeAt (for
// the wake word) and SplitOn (for the dictation-gate speak token).
func phrasesAt(words []string, i int, phrases [][]string) (consumed int) {
	for _, phrase := range phrases {
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
	// Prefer a number right after "last" ("read last three"), but for a read/replay
	// utterance any number word is the count — so bare "replay three" works too.
	for i, w := range words {
		if strings.Trim(strings.ToLower(w), ",.!?") == "last" && i+1 < len(words) {
			if n := wordToNum(words[i+1]); n > 0 {
				return n
			}
		}
	}
	for _, w := range words {
		if n := wordToNum(w); n > 0 {
			return n
		}
	}
	return 1
}

// spawnAgentWords maps a spoken backend name to its agent id for inline spawn
// selection. Only distinctive, non-path-like names belong here: "claude" is
// intentionally absent — it's the default backend AND a common path token (dirs
// like claude_spawner), so treating it as a selector would corrupt locations.
var spawnAgentWords = map[string]string{"codex": "codex", "opencode": "opencode"}

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

// extractSpawnProfile pulls an inline execution-profile choice out of a spawn
// utterance and returns the spoken profile phrase (empty if none) plus the
// utterance with that phrase removed, so name/location parse cleanly. "profile"
// is the anchor word: the run of ordinary words right before it is the profile
// ("with sandbox profile" -> "sandbox", "bare metal profile" -> "bare metal"), or
// the single word right after it in the "profile <name>" form. A leading
// "with"/"using" is dropped too. The profile name is left for the gateway to
// resolve against the registry (unknown -> the default profile).
func extractSpawnProfile(t string) (profile, rest string) {
	words := strings.Fields(t)
	for i, w := range words {
		if w != "profile" && w != "profiles" {
			continue
		}
		drop := map[int]bool{i: true}
		// A "with"/"using" before "profile" (with no other boundary in between)
		// bounds a possibly multi-word name: "with bare metal profile".
		withAt := -1
		for k := i - 1; k >= 0; k-- {
			if words[k] == "with" || words[k] == "using" {
				withAt = k
				break
			}
			if isSpawnBoundary(words[k]) {
				break
			}
		}
		switch {
		case withAt >= 0 && withAt < i-1: // "with <name…> profile"
			profile = strings.Join(words[withAt+1:i], " ")
			for k := withAt; k < i; k++ {
				drop[k] = true
			}
		case i-1 >= 0 && !isSpawnBoundary(words[i-1]): // single word "sandbox profile"
			profile = words[i-1]
			drop[i-1] = true
		case i+1 < len(words): // "profile <name>" form
			profile = words[i+1]
			drop[i+1] = true
			if i-1 >= 0 && (words[i-1] == "with" || words[i-1] == "using") {
				drop[i-1] = true
			}
		}
		out := make([]string, 0, len(words))
		for k, ww := range words {
			if !drop[k] {
				out = append(out, ww)
			}
		}
		return profile, strings.Join(out, " ")
	}
	return "", t
}

// isSpawnBoundary reports whether w is a spawn-grammar keyword that bounds a
// spoken name or profile phrase (a preposition, selector, or filler word), so the
// extractors don't swallow it into the value.
func isSpawnBoundary(w string) bool {
	switch w {
	case "in", "at", "under", "inside", "on", "using", "with",
		"called", "named", "session", "project", "new", "a", "an", "the",
		"spawn", "create", "start":
		return true
	}
	return false
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

// beforeAny returns everything preceding the first occurrence of any keyword (as
// a whole word), e.g. beforeAny("bug fix in data", "in") -> "bug fix". Returns the
// whole string if no keyword is present.
func beforeAny(t string, keywords ...string) string {
	words := strings.Fields(t)
	for i, w := range words {
		for _, kw := range keywords {
			if w == kw {
				return strings.Join(words[:i], " ")
			}
		}
	}
	return t
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

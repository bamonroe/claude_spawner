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
	Spawn    Kind = "spawn"
	Attach   Kind = "attach"
	Detach   Kind = "detach"
	List     Kind = "list"
	Kill     Kind = "kill"
	Status   Kind = "status"
	Cancel   Kind = "cancel"
	Stop     Kind = "stop"      // stop speaking (barge-in)
	Help     Kind = "help"      // list available commands
	ReadLast Kind = "read_last" // re-read the last N Claude replies aloud
	Unknown  Kind = "unknown"
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
	Count    int // for ReadLast: how many recent Claude replies to re-read
}

// wakeSeconds are the accepted second words after "hey" (incl. common whisper
// mishearings of "buddy").
var wakeSeconds = map[string]bool{
	"buddy": true, "bud": true, "body": true, "buddie": true, "budy": true,
}

// StripWake removes an optional leading wake phrase ("hey buddy" / "hey bud",
// with optional punctuation) and reports whether it was present. Used to
// distinguish control commands from plain dictation while attached.
func StripWake(text string) (rest string, hadWake bool) {
	words := strings.Fields(strings.TrimSpace(text))
	if len(words) >= 2 {
		if wakeAt(words, 0) {
			return strings.Join(words[2:], " "), true
		}
	}
	return strings.TrimSpace(text), false
}

// SplitWake locates the wake phrase ("hey buddy") in the text and splits into
// dictation + command. If "hey buddy" appears MULTIPLE times (the user
// self-corrected the command), the LAST one wins: before = text preceding the
// FIRST wake (the dictation), after = text following the LAST wake (the command);
// anything in between (earlier command attempts) is discarded.
func SplitWake(text string) (before, after string, found bool) {
	words := strings.Fields(strings.TrimSpace(text))
	first, last := -1, -1
	for i := 0; i+1 < len(words); i++ {
		if wakeAt(words, i) {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return strings.TrimSpace(text), "", false
	}
	return strings.Join(words[:first], " "), strings.Join(words[last+2:], " "), true
}

// wakeAt reports whether words[i], words[i+1] form a wake phrase ("hey" + a
// buddy variant), tolerating surrounding punctuation.
func wakeAt(words []string, i int) bool {
	if i+1 >= len(words) {
		return false
	}
	first := strings.Trim(strings.ToLower(words[i]), ",.!?")
	second := strings.Trim(strings.ToLower(words[i+1]), ",.!?")
	return first == "hey" && wakeSeconds[second]
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

	// Spawn: "spawn … session/project", or a leading new-session/project phrase.
	case first == "spawn" && contains(t, "session", "project"),
		leadsWith(t, "new session", "new project", "create a session", "create a project", "start a session", "start a project"):
		return Intent{
			Kind:     Spawn,
			New:      contains(t, "new project", "new repo", "new folder", "create a project", "start a project"),
			Location: afterAny(t, "in", "at", "under", "inside"),
		}

	// Detach: bare "detach"/"detach now", or an explicit phrase.
	case first == "detach" && n <= 2, contains(t, "stop dictating", "stop listening"):
		return Intent{Kind: Detach}

	// Kill: short "kill <name>", or an explicit "… session" phrase.
	case first == "kill" && n <= 3, contains(t, "kill session", "stop session", "end session", "close session"):
		return Intent{Kind: Kill, Arg: argAfter(t, "session", "kill")}

	// Attach: "attach to <name>".
	case first == "attach" && word2 == "to" && n <= 5, contains(t, "attach to"):
		return Intent{Kind: Attach, Arg: argAfter(t, "to", "attach")}

	// List: "list" optionally followed by a session qualifier.
	case first == "list" && (n == 1 || listQualifiers[word2]),
		contains(t, "what sessions", "which sessions", "sessions are"):
		return Intent{Kind: List}

	// Status: bare "status", or an explicit phrase.
	case first == "status" && n <= 2, contains(t, "what's it doing", "whats it doing", "what is it doing"):
		return Intent{Kind: Status}

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

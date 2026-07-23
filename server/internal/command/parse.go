package command

import "strings"

// Parse classifies an utterance (already wake-stripped) into an intent.
//
// Matching is deliberately precise, not loose substring containment: a control
// command is a SHORT utterance led by a command verb (or a distinctive phrase),
// so dictation like "list the files in this module" or "spawn a goroutine" is
// NOT misread as a command and instead flows through to Claude. Stop/Cancel stay
// broad because barge-in / dialog-abort must fire in any context.
//
// Parse is a short dispatcher over the parseX helpers below, each detecting one
// command family. They are tried IN ORDER and the first match wins — order is
// behaviorally significant (e.g. AbortTurn is checked before Cancel/Kill so
// "cancel the turn" doesn't fall through to Cancel).
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
	pc := parseCtx{t: t, words: words, first: first, n: n, word2: word2}

	for _, fn := range parsers {
		if intent, ok := fn(pc); ok {
			return intent
		}
	}
	return Intent{Kind: Unknown}
}

// parseCtx bundles the precomputed pieces of a normalized utterance that every
// parseX helper needs, so Parse tokenizes only once.
type parseCtx struct {
	t     string
	words []string
	first string
	n     int
	word2 string
}

// parsers is the ordered list of command-family matchers Parse dispatches to.
// The order MUST match the original monolithic Parse's switch order — it is
// behaviorally significant.
var parsers = []func(parseCtx) (Intent, bool){
	parseStop,
	parseAbortTurn,
	parseCancel,
	parseReadLast,
	parseHelp,
	parseListModels,
	parseUseModel,
	parseSpawn,
	parseDetach,
	parseSwap,
	parseSummaryOnly,
	parseSpeakEverything,
	parseScratch,
	parseListJobs,
	parseKillJob,
	parseJobStatus,
	parseRestart,
	parseKill,
	parseAttach,
	parseList,
	parseStatus,
	parseClear,
	parseCompress,
	parseUsage,
	parseRename,
}

// Barge-in / dialog abort: always eligible, regardless of length.
func parseStop(c parseCtx) (Intent, bool) {
	if c.t == "stop" || c.t == "quiet" || c.t == "hush" || c.t == "enough" ||
		contains(c.t, "stop talking", "stop speaking", "stop reading", "be quiet", "shut up") {
		return Intent{Kind: Stop}, true
	}
	return Intent{}, false
}

// Abort the running turn (kill the claude child). Checked before Cancel/Kill so
// "cancel the turn" / "kill the turn" abort the turn, not the message/session.
func parseAbortTurn(c parseCtx) (Intent, bool) {
	if c.t == "abort" || c.first == "abort" ||
		contains(c.t, "stop the turn", "stop the command", "stop the job", "stop the task",
			"stop working", "cancel the turn", "cancel the command", "kill the turn",
			"kill the command", "abort the turn", "halt the turn") {
		return Intent{Kind: AbortTurn}, true
	}
	return Intent{}, false
}

func parseCancel(c parseCtx) (Intent, bool) {
	if c.t == "cancel" || c.first == "cancel" ||
		contains(c.t, "cancel message", "cancel that", "never mind", "nevermind", "forget it", "scrap that", "scrap it") {
		return Intent{Kind: Cancel}, true
	}
	return Intent{}, false
}

// Read last: "read last", "read last 3", "read the last two", "read that
// back", "say that again", "repeat that/last", "replay last/that".
func parseReadLast(c parseCtx) (Intent, bool) {
	if (c.first == "read" && contains(c.t, "read last", "read the last", "read that", "read it", "read again")) ||
		c.first == "replay" ||
		contains(c.t, "say that again", "say it again", "repeat that", "repeat last", "read that back", "read it back", "replay last", "replay that") {
		return Intent{Kind: ReadLast, Count: readCount(c.words)}, true
	}
	return Intent{}, false
}

func parseHelp(c parseCtx) (Intent, bool) {
	if (c.first == "help" && c.n <= 2) || c.first == "commands" ||
		contains(c.t, "what can you do", "what can i say", "list commands", "show commands", "available commands", "which commands") {
		return Intent{Kind: Help}, true
	}
	return Intent{}, false
}

// Models: list the attached session's backend models. Checked before List/Status
// so "list models" isn't swallowed by the bare-"list" case.
func parseListModels(c parseCtx) (Intent, bool) {
	if leadsWith(c.t, "list models", "list the models", "show models", "show the models", "list available models") ||
		contains(c.t, "what models", "which models", "what are the models") {
		return Intent{Kind: ListModels}, true
	}
	return Intent{}, false
}

// switch to one by NUMBER ("use model 3"). Ordinal selection deliberately
// sidesteps hard-to-say model names (e.g. "gpt-5.5" reasoning presets).
func parseUseModel(c parseCtx) (Intent, bool) {
	if (c.first == "use" && contains(c.t, "model")) ||
		leadsWith(c.t, "switch to model", "switch model", "select model", "set model", "change model", "change to model", "pick model") {
		return Intent{Kind: UseModel, Count: modelIndex(c.words)}, true
	}
	return Intent{}, false
}

// Spawn: "spawn … session/project", or a leading new-session/project phrase.
func parseSpawn(c parseCtx) (Intent, bool) {
	if !((c.first == "spawn" && contains(c.t, "session", "project")) ||
		leadsWith(c.t, "new session", "new project", "create a session", "create a project", "start a session", "start a project")) {
		return Intent{}, false
	}
	// Pull the inline backend ("… on codex") and profile ("… with sandbox
	// profile") choices out first, so their words don't leak into the parsed
	// name/location.
	agentID, rest := extractSpawnAgent(c.t)
	profile, rest := extractSpawnProfile(rest)
	// A spoken session name: the words after "called"/"named", up to the
	// location preposition ("new session called bug fix in data" -> "bug fix").
	name := strings.TrimSpace(beforeAny(afterAny(rest, "called", "named"), "in", "at", "under", "inside"))
	// Prefer an explicit preposition ("spawn a session in git personal"); if
	// there's none AND no name was given, take whatever path was spoken right
	// after "session"/"project" ("spawn a new session bam git personal") so a
	// one-shot command with an inline location still jumps straight there.
	loc := afterAny(rest, "in", "at", "under", "inside")
	if loc == "" && name == "" {
		loc = afterAny(rest, "session", "project")
	}
	return Intent{
		Kind: Spawn,
		// Detect on the backend-stripped text so "new codex project" still reads
		// as new-project (the backend word sits between "new" and "project").
		New:      contains(rest, "new project", "new repo", "new folder", "create a project", "start a project"),
		Location: loc,
		Agent:    agentID,
		Profile:  profile,
		Name:     name,
	}, true
}

// Detach: bare "detach"/"detach now", or an explicit phrase.
func parseDetach(c parseCtx) (Intent, bool) {
	if (c.first == "detach" && c.n <= 2) || contains(c.t, "stop dictating", "stop listening") {
		return Intent{Kind: Detach}, true
	}
	return Intent{}, false
}

// Swap: toggle back to the previously attached session — a two-way jump
// between the current session and the one attached just before it. Bare
// "swap"/"swap back", or an explicit "previous/last session" phrase.
func parseSwap(c parseCtx) (Intent, bool) {
	if c.t == "swap" || (c.first == "swap" && c.n <= 3) ||
		contains(c.t, "swap back", "swap session", "swap to the last", "switch back",
			"previous session", "last session", "go back to the last", "go back to the previous") {
		return Intent{Kind: Swap}, true
	}
	return Intent{}, false
}

// SummaryOnly: speak only the final turn result, beeping through the
// intermediate streamed steps instead of reading each aloud. "summary
// only"/"summaries only"/"summary mode" turn it on (a trailing "off" on a
// summary phrase turns it off). The app's audio settings has the same toggle.
// Arg carries on/off.
func parseSummaryOnly(c parseCtx) (Intent, bool) {
	if !leadsWith(c.t, "summary only", "summaries only", "summary mode", "just the summary", "summarize only") {
		return Intent{}, false
	}
	arg := "on"
	if c.words[c.n-1] == "off" {
		arg = "off"
	}
	return Intent{Kind: SummaryOnly, Arg: arg}, true
}

// "speak everything"/"say everything"/"read everything" turn SummaryOnly off.
func parseSpeakEverything(c parseCtx) (Intent, bool) {
	if leadsWith(c.t, "speak everything", "say everything", "read everything", "speak it all", "read it all") {
		return Intent{Kind: SummaryOnly, Arg: "off"}, true
	}
	return Intent{}, false
}

// Scratch: toggle "scratch mode" — while detached, the server echoes each
// transcription back aloud so you can test STT quality. "scratch on"/"scratch
// off"/"scratch mode on/off"; bare "scratch" toggles. Arg carries on/off ("" =
// toggle).
func parseScratch(c parseCtx) (Intent, bool) {
	if !(c.first == "scratch" && c.n <= 3) && !leadsWith(c.t, "scratch mode", "scratch on", "scratch off") {
		return Intent{}, false
	}
	arg := ""
	last := c.words[c.n-1]
	switch last {
	case "on", "enable", "enabled", "start":
		arg = "on"
	case "off", "disable", "disabled", "stop", "end":
		arg = "off"
	}
	return Intent{Kind: Scratch, Arg: arg}, true
}

// Background jobs: list of the detached spawner-job jobs. Checked BEFORE the
// bare List/Kill/Status session cases so "list jobs" isn't swallowed by them.
func parseListJobs(c parseCtx) (Intent, bool) {
	if leadsWith(c.t, "list jobs", "list the jobs", "list background jobs", "list the background jobs",
		"show jobs", "show the jobs", "show background jobs", "background jobs") ||
		contains(c.t, "what jobs", "which jobs", "any jobs running") {
		return Intent{Kind: ListJobs}, true
	}
	return Intent{}, false
}

// KillJob needs a NUMBER so it can't be confused with killing a session or
// aborting the turn.
func parseKillJob(c parseCtx) (Intent, bool) {
	if ((c.first == "kill" || c.first == "stop" || c.first == "cancel") && contains(c.t, "job") && modelIndex(c.words) > 0) ||
		contains(c.t, "kill job", "stop job", "cancel job", "kill background job", "kill the job number") {
		return Intent{Kind: KillJob, Count: modelIndex(c.words)}, true
	}
	return Intent{}, false
}

func parseJobStatus(c parseCtx) (Intent, bool) {
	if leadsWith(c.t, "job status", "jobs status", "background job status", "background jobs status") ||
		contains(c.t, "status of the job", "status of jobs", "how are the jobs", "how's the job", "hows the job") {
		return Intent{Kind: JobStatus}, true
	}
	return Intent{}, false
}

// Restart: rebuild/restart the spawner server itself (fires SPAWNER_RESTART_CMD).
// Requires the "server"/"spawner" noun so it can't be confused with restarting a
// session, aborting a turn, or dictation. "restart the server", "rebuild the
// server", "restart/rebuild spawner".
func parseRestart(c parseCtx) (Intent, bool) {
	if ((c.first == "restart" || c.first == "rebuild" || c.first == "reboot") && contains(c.t, "server", "spawner")) ||
		contains(c.t, "restart the server", "rebuild the server", "reboot the server",
			"restart the spawner", "rebuild the spawner", "restart yourself", "rebuild yourself") {
		return Intent{Kind: Restart}, true
	}
	return Intent{}, false
}

// Kill: short "kill <name>", or an explicit "… session" phrase.
func parseKill(c parseCtx) (Intent, bool) {
	if (c.first == "kill" && c.n <= 3) || contains(c.t, "kill session", "stop session", "end session", "close session") {
		return Intent{Kind: Kill, Arg: argAfter(c.t, "session", "kill")}, true
	}
	return Intent{}, false
}

// Attach: "attach to <name>" — capture everything after "to" (multi-word dir
// names) and let the server fuzzy-match it.
func parseAttach(c parseCtx) (Intent, bool) {
	if !((c.first == "attach" && c.word2 == "to" && c.n <= 8) || contains(c.t, "attach to")) {
		return Intent{}, false
	}
	arg := afterAny(c.t, "to")
	if arg == "" {
		arg = afterAny(c.t, "attach")
	}
	return Intent{Kind: Attach, Arg: arg}, true
}

// List: "list" optionally followed by a session qualifier.
func parseList(c parseCtx) (Intent, bool) {
	if (c.first == "list" && (c.n == 1 || listQualifiers[c.word2])) ||
		contains(c.t, "what sessions", "which sessions", "sessions are") {
		return Intent{Kind: List}, true
	}
	return Intent{}, false
}

// Status: bare "status", or an explicit phrase.
func parseStatus(c parseCtx) (Intent, bool) {
	if (c.first == "status" && c.n <= 2) || contains(c.t, "what's it doing", "whats it doing", "what is it doing") {
		return Intent{Kind: Status}, true
	}
	return Intent{}, false
}

// Clear: rotate Claude's context to a fresh session_id. History is KEPT on
// disk for display — Claude just stops re-reading it. Deliberately does NOT
// match "clear history" (that would imply deletion, which this never does).
func parseClear(c parseCtx) (Intent, bool) {
	if (c.first == "clear" && (c.n == 1 || c.word2 == "context" || c.word2 == "session")) ||
		contains(c.t, "clear the context", "clear the session", "reset context",
			"reset the context", "fresh context", "start fresh", "wipe context") {
		return Intent{Kind: Clear}, true
	}
	return Intent{}, false
}

// Compress: summarize Claude's context, then rotate to a fresh session_id
// seeded with that summary — the /compact analogue of clear (context is
// condensed, not dropped). "compact"/"compress"/"condense"/"summarize the
// context" all match; like clear, history is KEPT on disk.
func parseCompress(c parseCtx) (Intent, bool) {
	if (c.first == "compress" && (c.n == 1 || c.word2 == "context" || c.word2 == "session")) ||
		(c.first == "compact" && (c.n == 1 || c.word2 == "context" || c.word2 == "session")) ||
		contains(c.t, "compress the context", "compact the context", "condense the context",
			"condense context", "summarize the context", "summarize context", "compact it") {
		return Intent{Kind: Compress}, true
	}
	return Intent{}, false
}

// Usage: report the Claude plan's usage limits (session/week % used, resets).
// Bare "usage", or phrasings asking how much is left / used.
func parseUsage(c parseCtx) (Intent, bool) {
	if (c.first == "usage" && c.n <= 2) ||
		contains(c.t, "how much usage", "usage left", "usage limit", "how much have i used",
			"how much is left", "check usage", "my usage", "show usage") {
		return Intent{Kind: Usage}, true
	}
	return Intent{}, false
}

// Rename: rename the currently-attached session. "rename to <name>", "rename
// this session <name>", "call this <name>", "name this session <name>". The
// new name is whatever follows the anchor keyword; the server sanitizes it to
// a single token, so multi-word is tolerated. Only parsed post-wake (the
// dispatch path only reaches Parse when a wake word was present), so the
// broad "call this"/"name this" phrasings can't hijack dictation.
func parseRename(c parseCtx) (Intent, bool) {
	if !(c.first == "rename" ||
		leadsWith(c.t, "call this", "name this") ||
		contains(c.t, "rename to", "rename this", "rename it", "rename session", "rename the session")) {
		return Intent{}, false
	}
	arg := afterAny(c.t, "to")
	if arg == "" {
		arg = afterAny(c.t, "session")
	}
	if arg == "" {
		arg = afterAny(c.t, "this", "it")
	}
	if arg == "" && c.first == "rename" {
		arg = afterAny(c.t, "rename")
	}
	return Intent{Kind: Rename, Arg: arg}, true
}

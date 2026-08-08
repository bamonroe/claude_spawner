package gateway

import (
	"log"
	"strings"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/session"
)

// dictate runs one Claude turn for the attached session as a background job that
// outlives this connection — so a long job keeps running if the app disconnects,
// and its result is delivered on reconnect. Only one turn per session at a time.
func (c *conn) dictate(text string) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	// Reconcile detached background jobs at the turn boundary — OFF this goroutine.
	// It's an SSH round-trip (up to its 8s timeout) and this is the connection's
	// serial read loop, so running it inline delayed the whole turn (and everything
	// queued behind it) before Claude was even started. Concurrent is safe by
	// construction: reconcileJobs claims the session's writer slot itself, and
	// startTurn below preempts it and waits for it to let go, so at most it loses
	// the race and its notes land on the NEXT turn instead of this one.
	go c.srv.reconcileJobs(c.attached, true)
	prompt := text
	// A prior "compress" left a compacted summary of the old context to carry into
	// this fresh session_id; prepend it to the FIRST dictation so Claude continues
	// with that condensed context. startTurn clears PendingSeed once the turn lands.
	if c.attached.PendingSeed != "" && !c.attached.Started {
		prompt = seedPreamble(c.attached.PendingSeed) + prompt
	}
	// Prepend any framed background-job completion notes so Claude learns a job it
	// started earlier has finished, then clear them (unconditionally — unlike the
	// compress seed, which is gated on !Started). stripInjected strips this back off
	// stored history so the echoed view stays clean.
	// Read them under the record's lock: the reconcile launched above may be
	// appending concurrently. Consumed notes are cleared BY COUNT after startTurn
	// claims the writer slot (below), so a note staged in between survives instead
	// of being wiped by a blanket nil.
	var notes []string
	c.attached.Read(func(s *session.Session) { notes = append(notes, s.PendingNotes...) })
	if len(notes) > 0 {
		prompt = jobNotesPreamble(notes) + prompt
	}
	if c.brief {
		// Opt-in: nudge Claude toward short, TTS-friendly replies. Only the prompt
		// to Claude carries the hint; the displayed/echoed transcript stays as spoken.
		prompt += briefSuffix
	}
	// Interactive mode: append the ask instruction only until it's been primed for
	// this context. Claude retains it across turns via --resume, so re-sending it
	// every turn just burns tokens; a `clear` resets AskPrimed to re-prime.
	primeAsk := c.interactive && !c.attached.AskPrimed
	if primeAsk {
		prompt += askInstruction // let Claude ask instead of guessing (parsed back on reply)
	}
	// Prime the background-job instruction once per context (like AskPrimed): tell
	// Claude to route long-running commands through spawner-job instead of
	// run_in_background, so they survive turns. Claude retains it via --resume;
	// clear/compress reset JobsPrimed to re-prime after a rotation (harmless).
	primeJobs := !c.attached.JobsPrimed
	if primeJobs {
		prompt += jobsInstruction(session.JobScriptPath(session.HostHome()))
	}
	if !c.srv.startTurn(c.attached, prompt, primeAsk, primeJobs) {
		c.send(msgSay("still working on the last one."))
		return
	}
	// The turn now owns the session's single writer slot (startTurn preempted any
	// reconcile and waited for it to release), so this is the safe point to drop the
	// notes we just injected — dropping exactly the ones consumed, by count.
	if len(notes) > 0 {
		n := len(notes)
		c.attached.Mutate(func(s *session.Session) {
			if n >= len(s.PendingNotes) {
				s.PendingNotes = nil
			} else {
				s.PendingNotes = append([]string(nil), s.PendingNotes[n:]...)
			}
		})
		if err := c.srv.store.Put(c.attached); err != nil {
			log.Printf("dictate[%s]: persist cleared notes: %v", c.attached.Name, err)
		}
	}
	// Mirror the prompt onto any other devices attached to this session.
	c.srv.echoUserPrompt(c.attached.SessionID, text, c)
}

// Scaffolding the server appends to a dictation before sending it to Claude. It's
// deliberately kept out of the live echo (dictate sends the raw text to other
// devices), so history — read back from Claude's transcript, which stores the
// augmented prompt — must strip it too (stripInjected) to match the live view.
const briefSuffix = "\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)"

// seedPreamble frames a compress summary as leading context ahead of the user's
// first dictation on the rotated session, so Claude treats it as the recap of the
// prior conversation rather than as a new instruction.
const (
	seedRecapOpen  = "[Continuing from a compacted session — recap of the conversation so far:]\n\n"
	seedRecapClose = "\n\n[End of recap. The user's message follows.]\n\n"
)

func seedPreamble(seed string) string {
	return seedRecapOpen + seed + seedRecapClose
}

// handoffRecapBudget bounds the verbatim history carried across a backend switch
// (doSetAgent). It keeps the most recent messages — the active working context —
// and elides older ones, so the recap is enough for real continuity without blowing
// the new backend's first-turn context. It runs through the same PendingSeed →
// seedPreamble path a compress summary does; the difference is only how the seed is
// produced (verbatim tail here vs. an LLM summary there).
const handoffRecapBudget = 16000

// formatHandoffRecap renders a session's transcript as a plain-text dialogue that
// seeds the next backend when the session switches AIs. It is backend-agnostic: the
// messages come from the generic transcriptReader (Driver.ReadTranscriptChain), and
// roles are labeled neutrally ("User"/"Assistant" rather than any backend's name) so
// the recap reads the same whichever AI produced it. Keeps the newest messages that
// fit handoffRecapBudget, marking older elided history. Returns "" when there's
// nothing to carry (empty chain, or a backend with no readable transcript).

// formatHandoffRecap renders a session's transcript as a plain-text dialogue that
// seeds the next backend when the session switches AIs. It is backend-agnostic: the
// messages come from the generic transcriptReader (Driver.ReadTranscriptChain), and
// roles are labeled neutrally ("User"/"Assistant" rather than any backend's name) so
// the recap reads the same whichever AI produced it. Keeps the newest messages that
// fit handoffRecapBudget, marking older elided history. Returns "" when there's
// nothing to carry (empty chain, or a backend with no readable transcript).
func formatHandoffRecap(msgs []session.Message) string {
	blocks := make([]string, 0, len(msgs))
	for _, m := range msgs {
		text := strings.TrimSpace(m.Text)
		if text == "" {
			continue
		}
		label := "Assistant"
		if m.Role == "user" {
			label = "User"
		}
		blocks = append(blocks, label+": "+text)
	}
	if len(blocks) == 0 {
		return ""
	}
	// Keep newest-first within budget, then restore chronological order.
	kept := make([]string, 0, len(blocks))
	total := 0
	elided := false
	for i := len(blocks) - 1; i >= 0; i-- {
		if len(kept) > 0 && total+len(blocks[i]) > handoffRecapBudget {
			elided = true
			break
		}
		kept = append(kept, blocks[i])
		total += len(blocks[i])
	}
	for l, r := 0, len(kept)-1; l < r; l, r = l+1, r-1 {
		kept[l], kept[r] = kept[r], kept[l]
	}
	recap := strings.Join(kept, "\n\n")
	if elided {
		recap = "[…earlier conversation elided…]\n\n" + recap
	}
	return recap
}

// stripInjected removes the server-appended prompt scaffolding — the brief-reply
// nudge, the interactive-mode ask instruction, the background-job notes and
// instruction, and any compress/handoff recap preamble — from a stored user
// message, so history shows exactly the text the user spoke. This keeps the
// history view consistent with the live echo (which never carried the
// scaffolding) and lets the app dedupe a replayed turn against its live copy.
//
// Order matters, and not for cosmetic reasons. The PREPENDED framed blocks come
// off first, because a recap is a verbatim transcript of earlier turns and those
// turns' prompts carried this very scaffolding — so the recap's interior contains
// perfectly real-looking copies of the suffix markers. Removing the self-delimited
// blocks first takes that quoted scaffolding with them, leaving only our own
// genuinely-trailing copies for the suffix trims to find.
func stripInjected(text string) string {
	// Job-completion notes and the compress/handoff recap are prepended as framed
	// open…close blocks; cut each back out wherever it sits.
	text = cutFramed(text, jobNotesOpen, jobNotesClose)
	text = cutFramed(text, seedRecapOpen, seedRecapClose)
	// The background-job instruction is a suffix like askInstruction but carries a
	// dynamic script path, so cut from its marker to the end rather than by exact
	// match — from the LAST occurrence, since it's appended last and any earlier one
	// is text the user (or a quoted turn) actually wrote. Do this before the
	// fixed-suffix trims: it may sit after them.
	if i := strings.LastIndex(text, strings.TrimSpace(jobsInstructionMark)); i >= 0 {
		text = strings.TrimRight(text[:i], " \t\r\n")
	}
	text = trimInjectedSuffix(text, askInstruction)
	text = trimInjectedSuffix(text, briefSuffix)
	// The autonomous job-completion turn (bgnotify) is WHOLLY synthetic — envelope +
	// notes + instruction, with no user words to preserve. Unlike the cases above,
	// which trim scaffolding off a real dictation, strip the entire row to empty so
	// history matches the live view (where the autonomous prompt shows no user bubble
	// at all); serveHistory then drops the now-empty row.
	if strings.HasPrefix(strings.TrimSpace(text), strings.TrimSpace(jobNotifyMark)) {
		return ""
	}
	return text
}

// cutFramed removes one server-injected `open`…`close` block from a stored user
// message, wherever in the text it sits.
//
// Both markers are matched on their bracketed sentence ALONE — with the blank
// lines they're declared with trimmed off — because the text comes back to us
// through a backend's transcript reader, and those readers reshape whitespace:
// opencode rejoins a message's text parts with its own separator, antigravity
// trims its <USER_REQUEST> envelope, and any of them may normalize newlines. The
// old prefix-anchored match ("does this start with exactly open?") missed as soon
// as a single byte of surrounding whitespace differed, and a missed strip doesn't
// degrade quietly: the entire recap — thousands of characters of prior
// conversation — renders in the chat log as if the user had spoken it.
//
// Matching the distinctive sentence instead makes the removal an invariant of the
// marker rather than of the exact bytes around it. If the closing marker is absent
// the text is returned untouched: without it there's no way to tell where the
// injected block ends and the user's own words begin, and dropping their message
// is worse than leaking ours.
func cutFramed(text, open, close string) string {
	o, c := strings.TrimSpace(open), strings.TrimSpace(close)
	i := strings.Index(text, o)
	if i < 0 {
		return text
	}
	rest := text[i+len(o):]
	j := strings.Index(rest, c)
	if j < 0 {
		return text
	}
	head := strings.TrimRight(text[:i], " \t\r\n")
	tail := strings.TrimLeft(rest[j+len(c):], " \t\r\n")
	if head != "" && tail != "" {
		return head + "\n\n" + tail
	}
	return head + tail
}

// trimInjectedSuffix drops a trailing scaffolding block, ignoring any whitespace
// drift the transcript round-trip introduced around it (same reasoning as
// cutFramed).
func trimInjectedSuffix(text, suffix string) string {
	s := strings.TrimSpace(suffix)
	if t := strings.TrimRight(text, " \t\r\n"); strings.HasSuffix(t, s) {
		return strings.TrimRight(t[:len(t)-len(s)], " \t\r\n")
	}
	return text
}

// jobsInstructionMark is the leading, path-free marker of jobsInstruction, used by
// stripInjected to remove the whole (dynamic-path) instruction from stored history.
const jobsInstructionMark = "\n\n[Background jobs] For any command that should keep running"

// affirmative / negative recognize yes/no style dialog replies. `extra` carries
// the connection's custom wake token so "<wake> yes" strips like "hey buddy yes".

// affirmative / negative recognize yes/no style dialog replies. `extra` carries
// the connection's custom wake token so "<wake> yes" strips like "hey buddy yes".
func affirmative(text string, extra [][]string) bool {
	r, _ := command.StripWakeWith(text, extra)
	return command.Parse(r).Kind != command.Cancel &&
		containsAny(r, "yes", "yeah", "yep", "yup", "sure", "do it", "please", "go ahead", "ok", "okay")
}

func negative(text string, extra [][]string) bool {
	r, _ := command.StripWakeWith(text, extra)
	return containsAny(r, "no", "nope", "nah", "don't", "do not", "scrap", "skip")
}

// newSession builds a durable record with a generated session_id, ensuring a
// unique name derived from base.

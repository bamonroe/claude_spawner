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
	// Reconcile detached background jobs at the turn boundary (before the prompt is
	// built) so a job that finished since the last turn gets its completion note
	// staged into PendingNotes now. Safe here: no turn is in flight yet, so this
	// doesn't race the running turn's own store.Put (the one-writer invariant).
	c.srv.reconcileJobs(c.attached, true)
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
	if len(c.attached.PendingNotes) > 0 {
		prompt = jobNotesPreamble(c.attached.PendingNotes) + prompt
		c.attached.PendingNotes = nil
		if err := c.srv.store.Put(c.attached); err != nil {
			log.Printf("dictate[%s]: persist cleared notes: %v", c.attached.Name, err)
		}
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
// nudge, the interactive-mode ask instruction, and any compress recap preamble —
// from a stored user message, so history shows exactly the text the user spoke.
// This keeps the history view consistent with the live echo (which never carried
// the scaffolding) and lets the app dedupe a replayed turn against its live copy.

// stripInjected removes the server-appended prompt scaffolding — the brief-reply
// nudge, the interactive-mode ask instruction, and any compress recap preamble —
// from a stored user message, so history shows exactly the text the user spoke.
// This keeps the history view consistent with the live echo (which never carried
// the scaffolding) and lets the app dedupe a replayed turn against its live copy.
func stripInjected(text string) string {
	// The background-job instruction is a suffix like askInstruction but carries a
	// dynamic script path, so strip from its marker to the end rather than by exact
	// match. Do this before the fixed-suffix trims (it may sit after them).
	if i := strings.Index(text, jobsInstructionMark); i >= 0 {
		text = text[:i]
	}
	text = strings.TrimSuffix(text, askInstruction)
	text = strings.TrimSuffix(text, briefSuffix)
	// Job-completion notes are prepended (parallel to the seed recap); strip that
	// framed block back off stored history.
	if strings.HasPrefix(text, jobNotesOpen) {
		if i := strings.Index(text, jobNotesClose); i >= 0 {
			text = text[i+len(jobNotesClose):]
		}
	}
	if strings.HasPrefix(text, seedRecapOpen) {
		if i := strings.Index(text, seedRecapClose); i >= 0 {
			text = text[i+len(seedRecapClose):]
		}
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

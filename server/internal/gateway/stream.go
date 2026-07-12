package gateway

import (
	"strings"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// gatedChunk buffers a hands-free clip's audio and fast-transcribes it (tiny
// model) just to show a live draft and watch for the end token. Nothing commits
// until the end token — then the WHOLE buffered audio is re-transcribed at once
// (with the user's chosen model) so whisper sees full context, not fragments.
func (c *conn) gatedChunk(pcm []byte) {
	if len(c.audioPCM)+len(pcm) <= maxHandsFreePCM {
		c.audioPCM = append(c.audioPCM, pcm...)
	} // else: end token never fired — stop growing; commit still uses what we have

	chunk, err := c.fastTranscriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
		transcribe.Options{Mode: "fixed", Model: "tiny"})
	if err != nil || strings.TrimSpace(chunk) == "" {
		return
	}
	c.buffer = append(c.buffer, chunk)
	joined := strings.Join(c.buffer, " ")
	// Instant barge-in: a pure "hey buddy stop" (nothing dictated before it) halts
	// the TTS the moment it shows up in the live draft — no end token required.
	// Guarded to a pure command so "…hey buddy stop the build" (dictation) isn't
	// swallowed; that still commits normally and routes as a Claude turn.
	if before, after, found := c.splitWake(joined); found && strings.TrimSpace(before) == "" {
		if command.Parse(command.ApplyAliases(after, c.aliases)).Kind == command.Stop {
			c.send(msgStopSpeaking())
			c.buffer = nil
			c.audioPCM = nil
			c.send(msgPending(""))
			return
		}
	}
	if _, _, found := splitEndToken(joined, c.endToken); !found {
		// Draft only what would actually be dictated: with the dictation gate on,
		// suppress pre-speak-token ambient speech so the note stays empty until the
		// gate opens (and can't grow unbounded from background chatter). Gate off:
		// gateDictation returns the text unchanged, so the draft is the full buffer.
		c.send(msgPending(c.gateDictation(joined)))
		return
	}
	c.commitMessage()
}

// commitMessage re-transcribes the whole buffered audio accurately, strips the
// end token, then routes it: an active dialog answer, a chain of "hey buddy"
// commands run in sequence (any "cancel" in the chain scraps the whole message),
// else dictation.
func (c *conn) commitMessage() {
	audio := c.audioPCM
	c.buffer = nil
	c.audioPCM = nil
	c.send(msgPending(""))
	if len(audio) == 0 {
		return
	}

	// The accurate re-transcribe below can take a beat; tell the app it's underway
	// so hands-free shows "transcribing…" rather than flashing back to "listening".
	c.send(msgTranscribing())

	full, err := c.transcriber().Transcribe(c.ctx, transcribe.PCM16WAV(audio, audioSampleRate, audioChannels),
		transcribe.Options{Mode: c.sttMode, Model: c.sttModel, Prompt: c.vocabBias()})
	if err != nil {
		c.fail("transcribe_failed", err.Error())
		return
	}
	msg, _, _ := splitEndToken(full, c.endToken) // drop the end token (+ any trailing)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		c.send(msgPending("")) // nothing recognized — clear the "transcribing…" state
		return
	}
	// Always echo the full recognized message as the user's bubble — so spoken
	// commands (not just dictation) show up in the chat.
	c.send(msgTranscript(msg, true))

	if c.dlg != nil { // a spawn dialog is in progress — the message is its answer
		c.handleDialog(msg)
		return
	}
	before, cmds := c.splitWakeAll(msg)
	if len(cmds) == 0 {
		if c.attached == nil {
			// Detached "safe mode": no session to dictate to, so the whole
			// utterance is a command — no "hey buddy" needed, and nothing can
			// leak into a Claude session.
			if !c.runCommand(command.Parse(command.ApplyAliases(msg, c.aliases))) {
				if c.scratch {
					c.send(msgSay(msg)) // scratch mode: read back exactly what was transcribed
				} else {
					c.send(msgSay("not a command — try 'list sessions' or 'attach to a session'."))
				}
			}
			return
		}
		if dict := c.gateDictation(msg); dict != "" { // attached: pure dictation (gated if enabled)
			c.dictate(dict)
		}
		return
	}
	// "<dictation> hey buddy <cmd> hey buddy <cmd> …": each wake starts a command.
	// Parse them all in order first.
	intents := make([]command.Intent, len(cmds))
	for i, seg := range cmds {
		intents[i] = command.Parse(command.ApplyAliases(seg, c.aliases))
	}
	// "cancel" scraps everything BEFORE it — the leading dictation and any earlier
	// commands — so you can self-correct mid-utterance and keep commanding after
	// it (the last cancel wins). A trailing cancel with nothing after it scraps the
	// whole utterance.
	intents, hadCancel := applyCancel(intents)
	if hadCancel {
		before = "" // the leading dictation precedes the cancel, so it's scrapped too
		if len(intents) == 0 {
			c.send(msgSay("scrapped it."))
			return
		}
	}
	// Run the commands in sequence; an unknown command is a no-op. Dictate the
	// leading fragment last, so a command like "attach" takes effect first and the
	// dictation lands in the just-attached session.
	for _, intent := range intents {
		c.runCommand(intent)
	}
	if before = strings.TrimSpace(before); before != "" && c.attached != nil {
		if dict := c.gateDictation(before); dict != "" {
			c.dictate(dict)
		}
	}
}

// applyCancel implements the "cancel" reset-point semantics for a chained
// utterance: a `cancel` intent scraps everything before it, so it returns only
// the intents following the LAST cancel (and whether any cancel was present).
// No cancel ⇒ the intents unchanged, false.
func applyCancel(intents []command.Intent) (kept []command.Intent, hadCancel bool) {
	last := -1
	for i, intent := range intents {
		if intent.Kind == command.Cancel {
			last = i
		}
	}
	if last < 0 {
		return intents, false
	}
	return intents[last+1:], true
}

// vocabBias returns a whisper initial-prompt that biases decoding toward the
// control vocabulary and this machine's session names, both of which STT
// otherwise mangles (so "hey buddy, attach to sfit" resolves). The command
// words come first (always present); session names are appended when any exist.
func (c *conn) vocabBias() string {
	vocab := command.Vocabulary()
	// Bias STT toward the client's custom wake token(s) and dictation-gate speak
	// token(s) too, so a non-"hey buddy" wake word and the speak marker survive
	// transcription and can actually match.
	for _, phrase := range c.wakePhrase {
		vocab = append(vocab, strings.Join(phrase, " "))
	}
	for _, phrase := range c.speakPhrase {
		vocab = append(vocab, strings.Join(phrase, " "))
	}
	parts := []string{"Commands: " + strings.Join(vocab, ", ") + "."}
	if sessions := c.srv.store.List(); len(sessions) > 0 {
		names := make([]string, 0, len(sessions))
		for _, s := range sessions {
			names = append(names, s.Name)
		}
		parts = append(parts, "Session names: "+strings.Join(names, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// clearBuffer discards the pending message + audio and clears the draft.
func (c *conn) clearBuffer() {
	c.buffer = nil
	c.audioPCM = nil
	c.send(msgPending(""))
}

// splitEndToken finds the (whole-word, case-insensitive) end token — which may
// be multiple words — and splits the text around it.
func splitEndToken(text, token string) (before, after string, found bool) {
	tok := strings.Fields(strings.ToLower(strings.TrimSpace(token)))
	if len(tok) == 0 {
		return text, "", false
	}
	words := strings.Fields(text)
	for i := 0; i+len(tok) <= len(words); i++ {
		match := true
		for j, tw := range tok {
			if strings.Trim(strings.ToLower(words[i+j]), ",.!?") != tw {
				match = false
				break
			}
		}
		if match {
			return strings.Join(words[:i], " "), strings.Join(words[i+len(tok):], " "), true
		}
	}
	return text, "", false
}

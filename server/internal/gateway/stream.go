package gateway

import (
	"log"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// gateIdleTimeout re-closes a speech gate that opened but never reached an end
// token — a detector false-positive on background speech would otherwise leave
// capture live indefinitely, dictating the room.
const gateIdleTimeout = 90 * time.Second

// gatedChunk buffers a hands-free clip's audio and fast-transcribes it (tiny
// model) just to show a live draft and watch for the end token. Nothing commits
// until the end token — then the WHOLE buffered audio is re-transcribed at once
// (with the user's chosen model) so whisper sees full context, not fragments.
//
// With the speech gate on, this runs in two modes. CLOSED: the clip is only
// scored for the gate phrase and retains nothing — no PCM, no draft, no
// transcript bubble, no accurate pass — so ambient chatter costs nothing and
// never reaches the app. OPEN (from the gate phrase to the end token): exactly
// the behaviour below. Gate off ⇒ always open, i.e. today's pipeline.
func (c *conn) gatedChunk(pcm []byte) {
	if c.gateActive() {
		if c.gateOpen && time.Since(c.gateOpenedAt) > gateIdleTimeout {
			log.Printf("speech gate: idle %s with no end token — re-closing, discarding buffer", gateIdleTimeout)
			c.clearBuffer() // also re-closes the gate
		}
		if !c.gateOpen && !c.openGate(pcm) {
			return // pre-gate speech: scored and dropped, nothing retained
		}
	}

	target := strings.TrimSpace(c.audioSessionID)
	if target != c.bufferSessionID && (len(c.audioPCM) > 0 || len(c.buffer) > 0) {
		c.buffer = nil
		c.audioPCM = nil
		c.send(msgPending(""))
	}
	c.bufferSessionID = target

	if len(c.audioPCM)+len(pcm) <= maxHandsFreePCM {
		c.audioPCM = append(c.audioPCM, pcm...)
	} // else: end token never fired — stop growing; commit still uses what we have

	// Score the end-token detector on THIS clip's raw audio up front, independent of
	// the fast transcript — a pure end-token clip ("beep beep") that the tiny model
	// renders as empty must still trip the gate, so the detector can't sit behind a
	// non-empty-transcript guard. ok=false ⇒ no detector / it errored.
	detFired, detOK := c.endTokenFired(pcm)

	chunk, err := c.fastTranscriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
		transcribe.Options{Mode: "fixed", Model: "tiny", Prompt: c.detectBias()})
	if err == nil && strings.TrimSpace(chunk) != "" {
		c.buffer = append(c.buffer, chunk)
	}
	joined := strings.Join(c.buffer, " ")
	// Instant barge-in: a pure "hey buddy stop" (nothing dictated before it) halts
	// the TTS the moment it shows up in the live draft — no end token required.
	// Guarded to a pure command so "…hey buddy stop the build" (dictation) isn't
	// swallowed; that still commits normally and routes as a Claude turn.
	if before, after, found := c.splitWake(joined); found && strings.TrimSpace(before) == "" {
		if command.Parse(command.ApplyAliases(after, c.aliases)).Kind == command.Stop {
			c.send(msgStopSpeaking())
			c.clearBuffer()
			return
		}
	}
	// End-token gate: commit when EITHER the purpose-trained detector fires on this
	// clip's audio (the "beep beep" token, where Whisper's string-match gives false
	// negatives) OR the configured text end token appears in the fast transcript (a
	// spoken token like "all set" that Whisper hears fine). The detector augments the
	// string-match; it doesn't replace it, so a custom end token keeps working.
	end := detOK && detFired
	if !end {
		_, _, end = command.SplitOn(joined, c.endPhrases())
	}
	if !end {
		// Draft only what will actually be sent: pre-gate speech never reaches the
		// buffer at all now, so this only trims the ambient tail sharing the clip
		// that opened the gate. Gate off: the draft is the full buffer.
		c.send(msgPending(c.stripGate(joined)))
		return
	}
	c.commitMessage()
}

// openGate scores a single clip against the speech gate while the gate is closed
// and reports whether it opened. Nothing about the clip is retained either way —
// with a gate detector configured the closed state never calls Whisper at all,
// which is the whole cost win; with no detector it falls back to the tiny fast
// pass used purely as a matcher whose transcript is thrown away.
//
// The one thing that stays ungated is barge-in: while TTS is playing, a pure
// "hey buddy stop" halts it without the gate phrase, because being unable to stop
// runaway speech without first addressing the machine is a trap. That costs the
// fast pass only while something is actually being spoken.
func (c *conn) openGate(pcm []byte) bool {
	fired, ok := c.detectorFired(pcm, c.gateModels())
	if ok && fired {
		c.armGate()
		return true
	}
	speaking := c.speaking()
	if ok && !speaking {
		return false // detector spoke, gate stayed shut, nothing to barge in on — no Whisper
	}
	text, err := c.fastTranscriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
		transcribe.Options{Mode: "fixed", Model: "tiny", Prompt: c.detectBias()})
	if err != nil {
		return false
	}
	if speaking {
		if before, after, found := c.splitWake(text); found && strings.TrimSpace(before) == "" {
			if command.Parse(command.ApplyAliases(after, c.aliases)).Kind == command.Stop {
				c.send(msgStopSpeaking())
				return false
			}
		}
	}
	if _, _, found := command.SplitOn(text, c.speakPhrases()); found {
		c.armGate()
		return true
	}
	return false
}

// armGate opens the gate for the utterance in flight. The clip that opened it is
// kept whole — it also holds the first words after the phrase — and the text-level
// strip in commitMessage drops everything up to and including the phrase, so the
// audio boundary can be sloppy while the dictated text stays exact.
func (c *conn) armGate() {
	c.gateOpen = true
	c.gateOpenedAt = time.Now()
	log.Printf("speech gate: opened — capturing until the end token")
}

// speaking reports whether TTS synthesis is in flight (the barge-in window).
func (c *conn) speaking() bool {
	c.speakMu.Lock()
	defer c.speakMu.Unlock()
	return c.speakCancel != nil
}

// endTokenFired reports whether this clip's audio trips the end-token detector.
// ok=false means no detector is configured, or it errored — the caller then falls
// back to the Whisper string-match, so a missing or flaky sidecar degrades
// gracefully (the A/B safety net). Detection is the ONLY job here; the accurate
// transcription still happens in commitMessage.
func (c *conn) endTokenFired(pcm []byte) (fired, ok bool) {
	return c.detectorFired(pcm, c.endModels())
}

// detectorFired scores this clip's audio against the given detector model keys —
// the shared path behind both the end token and the speech gate. ok=false means
// no detector is configured for this client, none of these tokens carries a
// model, or the sidecar errored; the caller then falls back to the Whisper
// string-match, so a missing or flaky sidecar degrades gracefully.
func (c *conn) detectorFired(pcm []byte, models []string) (fired, ok bool) {
	// The trained sidecar is opt-in per client: only score it when this connection
	// asked for the "detector" service. The default (and every other value, incl.
	// empty from an older client) stays on the always-present Whisper string-match —
	// so a server that has SPAWNER_WAKEWORD_URL set doesn't silently route everyone
	// through a detector whose model may not yet be trustworthy.
	if c.wakeService != "detector" || c.srv.detector == nil {
		return false, false
	}
	// A token with no model is Whisper-only, so it doesn't participate here. No
	// model configured ⇒ nothing to score, fall back to the string-match.
	if len(models) == 0 {
		return false, false
	}
	scores, err := c.srv.detector.Detect(c.ctx, pcm)
	if err != nil {
		log.Printf("wakeword: detect failed, falling back to whisper string-match: %v", err)
		return false, false
	}
	for _, m := range models {
		if scores[m] >= c.srv.wakeThreshold {
			log.Printf("wakeword: %s=%.4f fired (thr %.3f)", m, scores[m], c.srv.wakeThreshold)
			return true, true
		}
	}
	return false, true
}

// commitMessage re-transcribes the whole buffered audio accurately, strips the
// end token, then routes it: an active dialog answer, a chain of "hey buddy"
// commands run in sequence (any "cancel" in the chain scraps the whole message),
// else dictation.
func (c *conn) commitMessage() {
	audio := c.audioPCM
	sessionID := c.bufferSessionID
	gated := c.gateOpen
	c.gateOpen = false // every commit re-closes the gate: one utterance per opening
	c.buffer = nil
	c.audioPCM = nil
	c.bufferSessionID = ""
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
	msg, _, _ := command.SplitOn(full, c.endPhrases()) // drop the end token (+ any trailing)
	// The clip that opened the gate straddles the phrase, so the accurate transcript
	// still carries the tail of the pre-gate speech. Strip everything up to and
	// including the phrase here, once — after this the message is exactly what was
	// said to the machine, and commands and dictation route the same way ungated
	// speech always has.
	if gated {
		msg = c.stripGate(msg)
	}
	msg = strings.TrimSpace(msg)
	if msg == "" {
		c.send(msgPending("")) // nothing recognized — clear the "transcribing…" state
		return
	}
	// Always echo the full recognized message as the user's bubble — so spoken
	// commands (not just dictation) show up in the chat.
	c.send(msgTranscript(sessionID, c.srv.sessionName(sessionID), msg, true))

	if c.dlg != nil { // a spawn dialog is in progress — the message is its answer
		c.handleDialog(msg)
		return
	}
	if !c.selectClientSession(sessionID) {
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
		c.dictate(msg) // attached: pure dictation (already past the gate)
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
	// Honor spoken order: the leading dictation was spoken before the commands, so
	// dictate it FIRST — into whatever session is attached at that moment — then run
	// the commands. This lets "<dictation> hey buddy detach" land the dictation in
	// the session before the detach takes it away. (The trade-off is that
	// "<dictation> hey buddy attach" dictates into the *old* session, not the newly
	// attached one — spoken order wins.)
	if before = strings.TrimSpace(before); before != "" && c.attached != nil {
		c.dictate(before)
	}
	for _, intent := range intents {
		c.runCommand(intent)
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

// detectBias returns a whisper initial-prompt for the live detection pass that
// primes the tiny fast model toward the spoken tokens it must actually catch —
// the wake, end, and speak-gate phrases. On the live clip the fast model would
// otherwise decode blind and mangle these short phrases, so the wake word / end
// token miss even before the detector-or-string-match gate runs. Kept focused
// (no command vocabulary or session names) to hold the prompt short for the tiny
// model. Empty when no phrases are configured.
func (c *conn) detectBias() string {
	var phrases []string
	for _, set := range [][][]string{c.wakePhrases(), c.endPhrases(), c.speakPhrases()} {
		for _, phrase := range set {
			phrases = append(phrases, strings.Join(phrase, " "))
		}
	}
	if len(phrases) == 0 {
		return ""
	}
	return "Spoken tokens: " + strings.Join(phrases, ", ") + "."
}

// vocabBias returns a whisper initial-prompt that biases decoding toward the
// control vocabulary and this machine's session names, both of which STT
// otherwise mangles (so "hey buddy, attach to sfit" resolves). The command
// words come first (always present); session names are appended when any exist.
func (c *conn) vocabBias() string {
	vocab := command.Vocabulary()
	// Bias STT toward the configured wake and dictation-gate speak phrases (from the
	// spoken-token catalogue), so the wake word and the speak marker survive
	// transcription and can actually match — whatever they've been configured to.
	for _, phrase := range c.wakePhrases() {
		vocab = append(vocab, strings.Join(phrase, " "))
	}
	for _, phrase := range c.speakPhrases() {
		vocab = append(vocab, strings.Join(phrase, " "))
	}
	for _, phrase := range c.endPhrases() {
		vocab = append(vocab, strings.Join(phrase, " "))
	}
	parts := []string{"Commands: " + strings.Join(vocab, ", ") + "."}
	if sessions := c.srv.store.List(); len(sessions) > 0 {
		names := make([]string, 0, len(sessions))
		for _, s := range sessions {
			names = append(names, recName(s))
		}
		parts = append(parts, "Session names: "+strings.Join(names, ", ")+".")
	}
	return strings.Join(parts, " ")
}

// clearBuffer discards the pending message + audio, clears the draft, and
// re-closes the speech gate — an abandoned utterance must not leave the front
// door standing open for the next thing said in the room.
func (c *conn) clearBuffer() {
	c.buffer = nil
	c.audioPCM = nil
	c.gateOpen = false
	c.send(msgPending(""))
}

// End-token splitting now uses command.SplitOn over the configured end phrases
// (c.endPhrases()), which matches any of several end tokens whole-word — replacing
// the old single-token splitEndToken helper.

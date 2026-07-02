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
	c.audioPCM = append(c.audioPCM, pcm...)

	chunk, err := c.fastTranscriber().Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels),
		transcribe.Options{Mode: "fixed", Model: "tiny"})
	if err != nil || strings.TrimSpace(chunk) == "" {
		return
	}
	c.buffer = append(c.buffer, chunk)
	joined := strings.Join(c.buffer, " ")
	if _, _, found := splitEndToken(joined, c.endToken); !found {
		c.send(msgPending(joined))
		return
	}
	c.commitMessage()
}

// commitMessage re-transcribes the whole buffered audio accurately, strips the
// end token, then routes it: an active dialog answer, a mid-message buddy
// command (processed first; "cancel" scraps it), else dictation.
func (c *conn) commitMessage() {
	audio := c.audioPCM
	c.buffer = nil
	c.audioPCM = nil
	c.send(msgPending(""))
	if len(audio) == 0 {
		return
	}

	full, err := c.transcriber().Transcribe(c.ctx, transcribe.PCM16WAV(audio, audioSampleRate, audioChannels),
		transcribe.Options{Mode: c.sttMode, Model: c.sttModel})
	if err != nil {
		c.send(msgError("transcribe_failed", err.Error()))
		return
	}
	msg, _, _ := splitEndToken(full, c.endToken) // drop the end token (+ any trailing)
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}
	// Always echo the full recognized message as the user's bubble — so spoken
	// commands (not just dictation) show up in the chat.
	c.send(msgTranscript(msg, true))

	if c.dlg != nil { // a spawn dialog is in progress — the message is its answer
		c.handleDialog(msg)
		return
	}
	before, after, hadWake := command.SplitWake(msg)
	if !hadWake {
		if c.attached == nil {
			// Detached "safe mode": no session to dictate to, so the whole
			// utterance is a command — no "hey buddy" needed, and nothing can
			// leak into a Claude session.
			if !c.runCommand(command.Parse(command.ApplyAliases(msg, c.aliases))) {
				c.send(msgSay("not a command, bud — try 'list sessions' or 'attach to a session'."))
			}
			return
		}
		c.dictate(msg) // attached: pure dictation
		return
	}
	// "<dictation> hey buddy <command>": process the command first.
	intent := command.Parse(command.ApplyAliases(after, c.aliases))
	if intent.Kind == command.Cancel {
		c.send(msgSay("scrapped it, bud."))
		return
	}
	c.runCommand(intent) // unknown command is a no-op
	if before = strings.TrimSpace(before); before != "" && c.attached != nil {
		c.dictate(before)
	}
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

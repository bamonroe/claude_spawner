package gateway

// Server-side TTS: the `speak` request path (M2 of the Kokoro epic, see
// TODO.md). The client decides what gets spoken (mute/summary-only stay
// client-local) and sends `speak {id, text, voice?}` with markdown already
// stripped; the server synthesizes via the resident Kokoro server and streams
// back a `speak_audio {id, codec}` header, the audio bytes as binary frames,
// then `speak_end {id}`. Requests are serviced strictly in order by a single
// per-connection worker, so the frames between a header and its end always
// belong to that id — no per-frame tagging needed.

import (
	"context"
	"io"
	"log"
	"strings"
)

// speakReq is one queued TTS synthesis request from the client.
type speakReq struct {
	id     string
	text   string
	voice  string
	format string
}

// speakFormats is the allowlist of per-request response formats (the same set
// SPAWNER_TTS_FORMAT accepts); "" means the server default. Clients pick the
// encoding their playback path wants — pcm is raw 24 kHz s16le mono.
var speakFormats = map[string]bool{
	"": true, "mp3": true, "wav": true, "opus": true, "flac": true, "pcm": true,
}

// speakQueueLen bounds the per-connection backlog of speak requests; beyond it
// the server refuses with a speak_end error (the client falls back to
// on-device TTS) rather than buffering unboundedly.
const speakQueueLen = 32

// handleSpeak services an inbound `speak`: refuse it when server TTS can't
// run, else queue it for the connection's speak worker.
func (c *conn) handleSpeak(id, text, voice, format string) {
	if c.speakCh == nil {
		c.send(msgSpeakEnd(id, "tts disabled"))
		return
	}
	if strings.TrimSpace(text) == "" {
		c.send(msgSpeakEnd(id, "empty text"))
		return
	}
	if !speakFormats[format] {
		c.send(msgSpeakEnd(id, "bad format"))
		return
	}
	select {
	case c.speakCh <- speakReq{id: id, text: text, voice: voice, format: format}:
	default:
		c.send(msgSpeakEnd(id, "speak queue full"))
	}
}

// handleSpeakStop services an inbound `speak_stop` (barge-in): drop every
// queued request and abort the in-flight synthesis so no more frames follow.
// Dropped requests get no speak_end — the client that barged in already forgot
// them. No-op when server TTS is disabled or nothing is queued/playing.
func (c *conn) handleSpeakStop() {
	if c.speakCh == nil {
		return
	}
drain:
	for {
		select {
		case <-c.speakCh:
		default:
			break drain
		}
	}
	c.speakMu.Lock()
	cancel := c.speakCancel
	c.speakMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// handleTTSVoices relays Kokoro's voice catalogue (GET /v1/audio/voices) for
// the client's settings picker, plus the server-default voice. Answered from a
// goroutine so the (up to 10s) HTTP call never stalls the read loop.
func (c *conn) handleTTSVoices() {
	if c.srv.tts == nil {
		c.send(msgTTSVoices(nil, "", "tts disabled"))
		return
	}
	go func() {
		voices, err := c.srv.tts.Voices(c.ctx)
		if err != nil {
			log.Printf("tts: voices: %v", err)
			c.send(msgTTSVoices(nil, c.srv.tts.Voice, "voices unavailable"))
			return
		}
		c.send(msgTTSVoices(voices, c.srv.tts.Voice, ""))
	}()
}

// speakWorker drains the connection's speak queue, one synthesis at a time.
// Runs for the life of the connection: the channel is closed after the read
// loop exits, and the conn ctx aborts any in-flight synthesis.
func (c *conn) speakWorker() {
	for req := range c.speakCh {
		c.streamSpeak(req)
	}
}

// streamSpeak synthesizes one request and streams it to the client: the
// speak_audio header, the audio bytes as binary frames, then speak_end (with
// an error string when synthesis or the stream failed part-way).
func (c *conn) streamSpeak(req speakReq) {
	// Per-request cancel so a speak_stop barge-in aborts this synthesis without
	// touching the connection.
	ctx, cancel := context.WithCancel(c.ctx)
	c.speakMu.Lock()
	c.speakCancel = cancel
	c.speakMu.Unlock()
	defer func() {
		c.speakMu.Lock()
		c.speakCancel = nil
		c.speakMu.Unlock()
		cancel()
	}()
	body, _, err := c.srv.tts.Speak(ctx, req.text, req.voice, req.format)
	if err != nil {
		if ctx.Err() != nil { // barged in before synthesis started — not a failure
			c.send(msgSpeakEnd(req.id, "cancelled"))
			return
		}
		log.Printf("tts: speak: %v", err)
		c.send(msgSpeakEnd(req.id, "synthesis failed"))
		return
	}
	defer body.Close()
	codec := req.format
	if codec == "" {
		codec = c.srv.tts.Format
	}
	if c.send(msgSpeakAudio(req.id, codec)) != nil {
		return
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if c.sendBinary(buf[:n]) != nil {
				return // client gone; nobody left to report to
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil { // speak_stop barge-in aborted the stream
				c.send(msgSpeakEnd(req.id, "cancelled"))
				return
			}
			log.Printf("tts: stream: %v", rerr)
			c.send(msgSpeakEnd(req.id, "stream failed"))
			return
		}
	}
	c.send(msgSpeakEnd(req.id, ""))
}

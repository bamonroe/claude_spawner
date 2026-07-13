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
	"io"
	"log"
	"strings"
)

// speakReq is one queued TTS synthesis request from the client.
type speakReq struct {
	id    string
	text  string
	voice string
}

// speakQueueLen bounds the per-connection backlog of speak requests; beyond it
// the server refuses with a speak_end error (the client falls back to
// on-device TTS) rather than buffering unboundedly.
const speakQueueLen = 32

// handleSpeak services an inbound `speak`: refuse it when server TTS can't
// run, else queue it for the connection's speak worker.
func (c *conn) handleSpeak(id, text, voice string) {
	if c.speakCh == nil {
		c.send(msgSpeakEnd(id, "tts disabled"))
		return
	}
	if strings.TrimSpace(text) == "" {
		c.send(msgSpeakEnd(id, "empty text"))
		return
	}
	select {
	case c.speakCh <- speakReq{id: id, text: text, voice: voice}:
	default:
		c.send(msgSpeakEnd(id, "speak queue full"))
	}
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
	body, _, err := c.srv.tts.Speak(c.ctx, req.text, req.voice)
	if err != nil {
		log.Printf("tts: speak: %v", err)
		c.send(msgSpeakEnd(req.id, "synthesis failed"))
		return
	}
	defer body.Close()
	if c.send(msgSpeakAudio(req.id, c.srv.tts.Format)) != nil {
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
			log.Printf("tts: stream: %v", rerr)
			c.send(msgSpeakEnd(req.id, "stream failed"))
			return
		}
	}
	c.send(msgSpeakEnd(req.id, ""))
}

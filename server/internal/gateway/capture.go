package gateway

import (
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// Capture metadata: every whisper request the gateway makes carries a
// transcribe.Capture describing it, so the archiving proxy in front of whisper
// (voice-capture) can label each clip it keeps. The point is comparability: all
// the requests made for one spoken utterance — the closed-gate checks, each live
// partial, and the accurate final — share one utterance id and a running seq, so
// the archive can line the cheap live passes up against the final transcript of
// the same speech.
//
// An utterance is:
//   - push-to-talk / calibration: the single wake…audio_end clip;
//   - hands-free: every VAD-gated clip from the first one buffered until the
//     commit (or the buffer is discarded). A closed-gate clip that does NOT open
//     the gate is its own one-request utterance; the clip that opens it starts the
//     utterance the following partials and final belong to.
//
// All transcription goes through transcribeClip, so no call site can forget to
// stamp it.

// utterance is the capture identity shared by every request of one utterance.
type utterance struct {
	id        string
	seq       int // requests made so far; the next one is seq+1
	startedAt time.Time
	location  *transcribe.Location
	codec     string
	handsFree bool
	denoised  bool // every clip in the utterance was denoised
	sessionID string
}

// newUtterance starts an utterance at the current clip.
func (c *conn) newUtterance(pcm []byte, denoised bool) *utterance {
	id, err := session.NewSessionID()
	if err != nil {
		id = newTurnID()
	}
	return &utterance{
		id:        id,
		startedAt: clipStart(c.clipWakeAt, c.clipEndAt, pcm),
		location:  c.clipLocation,
		codec:     c.audioCodec,
		handsFree: c.gated,
		denoised:  denoised,
		sessionID: c.audioSessionID,
	}
}

// clipStart estimates when a clip began being spoken: the `wake` arrival, unless
// the client only sent `wake` once the clip was already recorded (the Ogg/Opus
// path ships the whole clip on release), in which case `audio_end` minus the
// clip's duration.
func clipStart(wakeAt, endAt time.Time, pcm []byte) time.Time {
	if endAt.IsZero() {
		endAt = time.Now()
	}
	start := endAt.Add(-time.Duration(float64(len(pcm)) / float64(audioSampleRate*2) * float64(time.Second)))
	if !wakeAt.IsZero() && wakeAt.Before(start) {
		return wakeAt
	}
	return start
}

// handsFreeUtterance returns the hands-free utterance the current clip belongs
// to, starting one if none is in flight, and folds the clip into it.
func (c *conn) handsFreeUtterance(pcm []byte, denoised bool) *utterance {
	u := c.handsFreeUtt
	if u == nil {
		u = c.newUtterance(pcm, denoised)
		c.handsFreeUtt = u
		return u
	}
	u.denoised = u.denoised && denoised
	if u.location == nil {
		u.location = c.clipLocation
	}
	return u
}

// toCapture converts the wire position fix to the transcribe form (nil-safe).
func (l *wakeLocation) toCapture() *transcribe.Location {
	if l == nil {
		return nil
	}
	loc := &transcribe.Location{Lat: l.Lat, Lon: l.Lon, AltM: l.AltM, AccuracyM: l.AccuracyM, Provider: l.Provider}
	if l.FixAt > 0 {
		loc.FixAt = time.UnixMilli(l.FixAt)
	}
	return loc
}

// transcribeClip is the gateway's one door to whisper: it picks the accurate or
// fast server, stamps the request with the utterance's capture metadata (next
// seq), and transcribes pcm. denoised reports whether THIS audio was scrubbed.
func (c *conn) transcribeClip(u *utterance, kind string, fast bool, pcm []byte, denoised bool, opt transcribe.Options) (string, error) {
	stt, role := c.transcriber(), "accurate"
	if fast && c.srv.fastStt != nil {
		stt, role = c.srv.fastStt, "fast"
	}
	u.seq++
	opt.Capture = c.capture(u, kind, role, denoised)
	return stt.Transcribe(c.ctx, transcribe.PCM16WAV(pcm, audioSampleRate, audioChannels), opt)
}

// capture builds the metadata for one request of utterance u.
func (c *conn) capture(u *utterance, kind, role string, denoised bool) *transcribe.Capture {
	cp := &transcribe.Capture{
		Kind:        kind,
		UtteranceID: u.id,
		Seq:         u.seq,
		RecordedAt:  u.startedAt,
		Location:    u.location,
		ClientID:    c.clientID,
		Platform:    c.platform,
		AppVersion:  c.appVersion,
		Device:      c.device,
		Codec:       u.codec,
		HandsFree:   u.handsFree,
		Denoised:    denoised,
		STTRole:     role,
	}
	if c.srv.cfg != nil {
		cp.Artist = c.srv.cfg.CaptureArtist
	}
	model, fastModel := c.srv.currentWhisperModels()
	if role == "fast" {
		model = fastModel
	}
	cp.STTModel = model
	// The session the speech is addressed to: the clip's declared target, else
	// whatever this connection is attached to.
	var rec *session.Session
	if u.sessionID != "" && c.srv.store != nil {
		rec = c.srv.store.GetBySessionID(u.sessionID)
	}
	if rec == nil {
		rec = c.attached
	}
	if rec != nil {
		rec.Read(func(r *session.Session) {
			cp.SessionID, cp.SessionName, cp.SessionCwd, cp.Host = r.SessionID, r.Name, r.Dir, r.Host
		})
		if cp.Host == "" {
			cp.Host = "local"
		}
	} else {
		cp.SessionID = u.sessionID
	}
	return cp
}

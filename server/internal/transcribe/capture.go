package transcribe

import (
	"encoding/binary"
	"strconv"
	"time"
)

// Capture kinds: what a single transcription request is FOR. Every request made
// for one spoken utterance shares a Capture.UtteranceID, so an archive of clips
// can line the cheap live passes up against the final transcript of the same
// speech.
const (
	// CaptureFinal is the transcript that is acted on: a push-to-talk clip, or
	// the whole buffered hands-free utterance re-transcribed accurately on commit.
	CaptureFinal = "final"
	// CapturePartial is one hands-free clip's fast pass, which feeds the live
	// draft and the end-token string-match.
	CapturePartial = "partial"
	// CaptureGateCheck is a fast pass made only to listen for the speech-gate
	// phrase while the gate is closed (its transcript is otherwise discarded).
	CaptureGateCheck = "gatecheck"
	// CaptureCalibration is an end-token calibration probe (reported, not acted on).
	CaptureCalibration = "calibration"
)

// CaptureSource identifies this program as the origin of a request.
const CaptureSource = "claude_spawner"

// Location is the device's last position fix at the start of an utterance.
type Location struct {
	Lat, Lon  float64
	AltM      *float64 // nil = the fix had no altitude
	AccuracyM *float64 // nil = the fix had no accuracy estimate
	FixAt     time.Time
	Provider  string // e.g. "gps", "network", "fused"
}

// Capture is provenance metadata for one transcription request: who/where/what
// produced the audio and why it is being transcribed. A transcriber that talks to
// an archiving proxy (RemoteWhisper → voice-capture) sends it as `capture_*`
// multipart fields, which the proxy embeds in the archived WAV; engines with no
// such sink (WhisperCPP) ignore it. Zero-valued string fields are omitted.
type Capture struct {
	Kind        string // CaptureFinal | CapturePartial | CaptureGateCheck | CaptureCalibration
	UtteranceID string // shared by every request of one utterance
	Seq         int    // 1-based request number within the utterance
	Artist      string // who is speaking (SPAWNER_CAPTURE_ARTIST)
	RecordedAt  time.Time
	Location    *Location

	ClientID   string
	Platform   string // "android" | "web"
	AppVersion string
	Device     string

	SessionID   string
	SessionName string
	SessionCwd  string
	Host        string

	Codec     string // the codec the client sent ("ogg_opus" | "pcm16")
	HandsFree bool
	Denoised  bool   // the audio sent was scrubbed by the denoise sidecar
	STTRole   string // "accurate" | "fast": which resident whisper server
	STTModel  string // the model loaded on that server, when known
}

// rfc3339 formats t with its zone offset (seconds precision), "" for the zero time.
func rfc3339(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

func ftoa(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// FormFields renders the capture as ordered `capture_*` form fields for the
// request carrying wav. The clip duration is measured from wav itself, so it is
// always the duration of exactly the audio sent. Empty strings are omitted;
// booleans are always present once a Capture is attached.
func (c *Capture) FormFields(wav []byte) [][2]string {
	if c == nil {
		return nil
	}
	var out [][2]string
	add := func(k, v string) {
		if v != "" {
			out = append(out, [2]string{"capture_" + k, v})
		}
	}
	add("source", CaptureSource)
	add("kind", c.Kind)
	add("utterance", c.UtteranceID)
	if c.Seq > 0 {
		add("seq", strconv.Itoa(c.Seq))
	}
	add("artist", c.Artist)
	add("recorded_at", rfc3339(c.RecordedAt))
	if l := c.Location; l != nil {
		add("lat", ftoa(l.Lat))
		add("lon", ftoa(l.Lon))
		if l.AltM != nil {
			add("alt_m", ftoa(*l.AltM))
		}
		if l.AccuracyM != nil {
			add("accuracy_m", ftoa(*l.AccuracyM))
		}
		add("location_at", rfc3339(l.FixAt))
		add("location_provider", l.Provider)
	}
	add("client_id", c.ClientID)
	add("platform", c.Platform)
	add("app_version", c.AppVersion)
	add("device", c.Device)
	add("session_id", c.SessionID)
	add("session_name", c.SessionName)
	add("session_cwd", c.SessionCwd)
	add("host", c.Host)
	add("codec", c.Codec)
	add("hands_free", strconv.FormatBool(c.HandsFree))
	add("denoised", strconv.FormatBool(c.Denoised))
	add("stt_role", c.STTRole)
	add("stt_model", c.STTModel)
	if d := WAVDuration(wav); d > 0 {
		add("duration_s", strconv.FormatFloat(d, 'f', 3, 64))
	}
	return out
}

// WAVDuration returns the length in seconds of a canonical 44-byte-header PCM
// WAV (as PCM16WAV builds), from its declared byte rate. Anything else falls back
// to the 16 kHz mono PCM16 contract (32000 bytes/s).
func WAVDuration(wav []byte) float64 {
	if len(wav) >= 44 && string(wav[0:4]) == "RIFF" && string(wav[8:12]) == "WAVE" {
		if rate := binary.LittleEndian.Uint32(wav[28:32]); rate > 0 {
			return float64(len(wav)-44) / float64(rate)
		}
	}
	return float64(len(wav)) / 32000.0
}

// Package detect scores a bounded audio clip for the presence of the wake and
// end tokens using the purpose-trained keyword-spotting sidecar (spawner-wakeword,
// wrapping LiveKit's runtime), instead of fast-transcribing the clip with Whisper
// and string-matching the transcript.
//
// The detector is a GATE, not a transcriber: it answers yes/no on a clip —
// "is a command starting?" (the wake token) and, the high-value one, "was the end
// token spoken?" (where Whisper's string-match gives false negatives today). When
// the end token fires, the gateway hands the whole accumulated utterance to
// accurate Whisper for the real parse. The detector never produces command text.
package detect

import "context"

// Model names — the sidecar's score-map keys (the classifiers it was trained on).
const (
	WakeModel = "bump_bump" // start/wake token ("bump bump")
	EndModel  = "beep_beep" // end token ("beep beep")
)

// Scores maps a model name to its peak confidence in [0,1] for a clip.
type Scores map[string]float64

// Detector scores a raw PCM16LE 16 kHz mono clip for the wake/end tokens.
type Detector interface {
	Detect(ctx context.Context, pcm []byte) (Scores, error)
}

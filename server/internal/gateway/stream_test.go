package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/command"
	"github.com/bam/claude_spawner/server/internal/detect"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/spoken"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// stubDetector returns canned scores (or an error) for endTokenFired tests.
type stubDetector struct {
	scores detect.Scores
	err    error
}

func (s stubDetector) Detect(context.Context, []byte) (detect.Scores, error) {
	return s.scores, s.err
}

func TestEndTokenFired(t *testing.T) {
	cases := []struct {
		name      string
		service   string // per-client wake backend; "detector" opts into the sidecar
		detector  detect.Detector
		threshold float64
		wantFired bool
		wantOK    bool
	}{
		// No detector → ok=false so the caller falls back to the whisper string-match.
		{"nil-detector", "detector", nil, 0.5, false, false},
		// End score above threshold → fired.
		{"above", "detector", stubDetector{scores: detect.Scores{detect.EndModel: 0.91, detect.WakeModel: 0.02}}, 0.5, true, true},
		// End score below threshold → not fired, but still ok (detector spoke).
		{"below", "detector", stubDetector{scores: detect.Scores{detect.EndModel: 0.10}}, 0.5, false, true},
		// Exactly at threshold counts as fired (>=).
		{"at-threshold", "detector", stubDetector{scores: detect.Scores{detect.EndModel: 0.5}}, 0.5, true, true},
		// Low threshold (the tuned operating point) catches a marginal token.
		{"low-threshold", "detector", stubDetector{scores: detect.Scores{detect.EndModel: 0.06}}, 0.04, true, true},
		// Detector error → ok=false, graceful fallback to whisper.
		{"error", "detector", stubDetector{err: errors.New("sidecar down")}, 0.5, false, false},
		// Default whisper client: never score the sidecar even when one is configured.
		{"whisper-default", "", stubDetector{scores: detect.Scores{detect.EndModel: 0.99}}, 0.5, false, false},
		{"whisper-explicit", "whisper", stubDetector{scores: detect.Scores{detect.EndModel: 0.99}}, 0.5, false, false},
	}
	for _, c := range cases {
		cn := &conn{ctx: context.Background(), wakeService: c.service, srv: &Server{detector: c.detector, wakeThreshold: c.threshold, tokens: testTokens(t)}}
		fired, ok := cn.endTokenFired([]byte{0, 0})
		if fired != c.wantFired || ok != c.wantOK {
			t.Errorf("%s: endTokenFired = (%v,%v), want (%v,%v)", c.name, fired, ok, c.wantFired, c.wantOK)
		}
	}
}

// gateModelKey is the detector model a speech-gate token is bound to in these
// tests (no trained gate model ships yet — the code path just has to accept one).
const gateModelKey = "speech_gate"

// stubTranscriber returns a canned transcript for every clip, and counts calls so
// a test can assert the closed gate never reached Whisper.
type stubTranscriber struct {
	text  string
	calls int
}

func (s *stubTranscriber) Transcribe(context.Context, []byte, transcribe.Options) (string, error) {
	s.calls++
	return s.text, nil
}

// gateTokens is the catalogue a gated connection runs with: a speech-gate phrase
// carrying a detector model, plus the default wake/end tokens.
func gateTokens(t *testing.T) *session.SpokenTokenStore {
	t.Helper()
	return tokensSeed(t, []*spoken.Token{
		{Name: "wake", Phrase: "hey buddy", Action: spoken.ActionWake},
		{Name: "end", Phrase: "all set", Action: spoken.ActionEnd},
		{Name: "note", Phrase: "take a note", Action: spoken.ActionSpeechGate, Model: gateModelKey},
	})
}

// TestGateFrontDoor: while the gate is closed, a clip is scored and NOTHING is
// retained — no PCM, no draft buffer. The gate phrase opens it, and from there
// capture behaves as it always has.
func TestGateFrontDoor(t *testing.T) {
	stt := &stubTranscriber{text: "just people talking nearby"}
	cn := &conn{ctx: context.Background(), dictationGate: true,
		srv: &Server{stt: stt, tokens: gateTokens(t)}}
	cn.gatedChunk([]byte{1, 2, 3, 4})
	if len(cn.audioPCM) != 0 || len(cn.buffer) != 0 || cn.gateOpen {
		t.Fatalf("closed gate retained state: pcm=%d buffer=%v open=%v", len(cn.audioPCM), cn.buffer, cn.gateOpen)
	}
	// Same connection, now the clip carries the gate phrase: the gate opens and the
	// straddling clip's audio is kept whole.
	stt.text = "take a note fix the bug"
	if !cn.openGate([]byte{1, 2}) {
		t.Fatal("gate phrase did not open the gate")
	}
	if !cn.gateOpen {
		t.Fatal("gate not marked open")
	}
}

// With a gate detector configured, a closed gate never calls Whisper at all —
// that's the cost win the front door exists for.
func TestGateClosedSkipsWhisperWithDetector(t *testing.T) {
	stt := &stubTranscriber{text: "chatter"}
	cn := &conn{ctx: context.Background(), dictationGate: true, wakeService: "detector",
		srv: &Server{stt: stt, tokens: gateTokens(t), wakeThreshold: 0.5,
			detector: stubDetector{scores: detect.Scores{gateModelKey: 0.01}}}}
	cn.gatedChunk([]byte{1, 2, 3, 4})
	if stt.calls != 0 {
		t.Fatalf("closed gate called whisper %d times, want 0", stt.calls)
	}
	if cn.gateOpen {
		t.Fatal("gate opened on a below-threshold score")
	}
	// Detector fires → gate opens, still without a Whisper pass to decide it.
	cn.srv.detector = stubDetector{scores: detect.Scores{gateModelKey: 0.99}}
	if !cn.openGate([]byte{1, 2}) || !cn.gateOpen {
		t.Fatal("detector above threshold did not open the gate")
	}
}

// An open gate that never sees an end token re-closes on the idle timeout, so a
// detector false positive can't leave capture live on the room.
func TestGateIdleTimeoutRecloses(t *testing.T) {
	stt := &stubTranscriber{text: "chatter"}
	cn := &conn{ctx: context.Background(), dictationGate: true,
		srv: &Server{stt: stt, tokens: gateTokens(t)}}
	cn.gateOpen = true
	cn.gateOpenedAt = time.Now().Add(-2 * gateIdleTimeout)
	cn.audioPCM = []byte{9, 9}
	cn.buffer = []string{"stale"}
	cn.gatedChunk([]byte{1, 2, 3, 4}) // clearBuffer sends msgPending — needs a ws, so stop before that
	if cn.gateOpen || len(cn.audioPCM) != 0 || len(cn.buffer) != 0 {
		t.Fatalf("idle gate not re-closed: open=%v pcm=%d buffer=%v", cn.gateOpen, len(cn.audioPCM), cn.buffer)
	}
}

// One phrase, both brackets: "pickle …message… pickle". The first occurrence opens
// the gate and is stripped from the draft, which leaves the second free to match as
// the end token — so the utterance commits instead of closing the instant it opened.
func TestGateAndEndSharePhrase(t *testing.T) {
	tokens := tokensSeed(t, []*spoken.Token{
		{Name: "wake", Phrase: "hey buddy", Action: spoken.ActionWake},
		{Name: "gate", Phrase: "pickle", Action: spoken.ActionSpeechGate},
		{Name: "end", Phrase: "pickle", Action: spoken.ActionEnd},
	})
	stt := &stubTranscriber{text: "pickle fix the parser bug"}
	cn := &conn{ctx: context.Background(), dictationGate: true, srv: &Server{stt: stt, tokens: tokens}}

	// Opening clip: gate opens, and the leading "pickle" must NOT read as the end
	// token — nothing commits yet.
	cn.gatedChunk([]byte{1, 2, 3, 4})
	if !cn.gateOpen {
		t.Fatal("shared phrase did not open the gate")
	}
	if len(cn.audioPCM) == 0 || len(cn.buffer) == 0 {
		t.Fatal("opening clip was not retained")
	}
	// The draft the client sees has the opening bracket stripped.
	if got := draftText(cn); got != "fix the parser bug" {
		t.Fatalf("draft = %q, want the gate phrase stripped", got)
	}
	// A second "pickle" — here in the same clip — is what closes it.
	cn.buffer = []string{"pickle yes lets do that pickle"}
	if _, _, found := command.SplitOn(draftText(cn), cn.endPhrases()); !found {
		t.Fatal("second occurrence did not match as the end token")
	}
}

// draftText is the draft text gatedChunk would send: the joined buffer past the
// gate phrase — the same value every downstream matcher sees.
func draftText(cn *conn) string {
	return cn.stripGate(strings.Join(cn.buffer, " "))
}

func TestStripGate(t *testing.T) {
	// Speech-gate tokens as they now live in the catalogue: "take a note" / "dictate".
	speak := []*spoken.Token{
		{Name: "note", Phrase: "take a note", Action: spoken.ActionSpeechGate},
		{Name: "dictate", Phrase: "dictate", Action: spoken.ActionSpeechGate},
	}
	cases := []struct {
		name   string
		gate   bool
		tokens []*spoken.Token
		in     string
		want   string
	}{
		// Gate off: text passes through verbatim (current behavior).
		{"off", false, speak, "some ambient chatter", "some ambient chatter"},
		// Gate on, phrase present: everything up to and including it is trimmed.
		{"bracketed", true, speak, "radio noise take a note fix the bug", "fix the bug"},
		{"variant", true, speak, "blah dictate ship it", "ship it"},
		// Gate on, no phrase in the text: the detector opened the gate, so there is
		// nothing to trim — pass it through rather than swallow the utterance.
		{"detector-opened", true, speak, "fix the bug", "fix the bug"},
		// Gate on but no speak token configured: fail safe — pass through, don't
		// silently swallow everything.
		{"no-token", true, nil, "still dictate this", "still dictate this"},
	}
	for _, c := range cases {
		cn := &conn{dictationGate: c.gate, srv: &Server{tokens: tokensSeed(t, c.tokens)}}
		if got := cn.stripGate(c.in); got != c.want {
			t.Errorf("%s: stripGate(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
	}
}

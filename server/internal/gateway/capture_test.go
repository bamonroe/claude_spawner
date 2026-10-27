package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// recordingSTT returns scripted transcripts in order and records each request's
// capture metadata.
type recordingSTT struct {
	texts []string
	got   []*transcribe.Capture
}

func (r *recordingSTT) Transcribe(_ context.Context, _ []byte, opt transcribe.Options) (string, error) {
	r.got = append(r.got, opt.Capture)
	if len(r.texts) == 0 {
		return "", nil
	}
	t := r.texts[0]
	r.texts = r.texts[1:]
	return t, nil
}

func testStore(t *testing.T) *session.Store {
	t.Helper()
	st, err := session.OpenStore(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestHandsFreeCaptureUtterance: every whisper request of one gated hands-free
// utterance — the gate check that opened it, each live partial, the accurate
// final — shares one utterance id with an incrementing seq, while a pre-gate clip
// that is dropped is its own utterance.
func TestHandsFreeCaptureUtterance(t *testing.T) {
	stt := &recordingSTT{texts: []string{
		"people talking nearby",   // clip 1: gate check, dropped
		"take a note fix the bug", // clip 2: gate check, opens (reused as the partial)
		"in the parser",           // clip 3: partial
		"all set",                 // clip 4: partial with the end token → commit
		"",                        // final: empty, so commit stops before routing
	}}
	cn := &conn{ctx: context.Background(), dictationGate: true, outDone: true,
		gated: true, audioCodec: codecOggOpus, clientID: "cid", platform: "android",
		appVersion: "9.9", device: "Pixel",
		srv: &Server{stt: stt, fastStt: stt, tokens: gateTokens(t), store: testStore(t),
			cfg: &config.Config{CaptureArtist: "Brian Monroe"}}}
	pcm := make([]byte, 3200) // 0.1 s

	cn.gatedChunk(pcm, false)
	t0 := time.Now().Add(-time.Minute)
	cn.clipWakeAt = t0
	cn.clipLocation = &transcribe.Location{Lat: 1, Lon: 2, Provider: "gps"}
	cn.gatedChunk(pcm, true)
	cn.clipWakeAt, cn.clipLocation = time.Now(), nil
	cn.gatedChunk(pcm, true)
	cn.gatedChunk(pcm, true)

	type want struct {
		kind, role string
		seq        int
	}
	wants := []want{
		{transcribe.CaptureGateCheck, "fast", 1},
		{transcribe.CaptureGateCheck, "fast", 1},
		{transcribe.CapturePartial, "fast", 2},
		{transcribe.CapturePartial, "fast", 3},
		{transcribe.CaptureFinal, "accurate", 4},
	}
	if len(stt.got) != len(wants) {
		t.Fatalf("got %d whisper requests, want %d", len(stt.got), len(wants))
	}
	for i, w := range wants {
		g := stt.got[i]
		if g == nil || g.Kind != w.kind || g.STTRole != w.role || g.Seq != w.seq {
			t.Fatalf("request %d = %+v, want %+v", i, g, w)
		}
		if g.Artist != "Brian Monroe" || g.ClientID != "cid" || g.Platform != "android" || g.AppVersion != "9.9" ||
			g.Device != "Pixel" || g.Codec != codecOggOpus || !g.HandsFree {
			t.Errorf("request %d identity = %+v", i, g)
		}
	}
	first, utt := stt.got[0].UtteranceID, stt.got[1].UtteranceID
	if first == "" || utt == "" || first == utt {
		t.Fatalf("dropped pre-gate clip should be its own utterance: %q vs %q", first, utt)
	}
	for i := 2; i < len(stt.got); i++ {
		if stt.got[i].UtteranceID != utt {
			t.Errorf("request %d utterance %q, want %q", i, stt.got[i].UtteranceID, utt)
		}
		if !stt.got[i].RecordedAt.Equal(t0) {
			t.Errorf("request %d recorded_at %v, want the opening clip's wake %v", i, stt.got[i].RecordedAt, t0)
		}
		if l := stt.got[i].Location; l == nil || l.Provider != "gps" {
			t.Errorf("request %d lost the utterance's location: %+v", i, l)
		}
	}
	if !stt.got[4].Denoised {
		t.Error("final of an all-denoised utterance should report denoised")
	}
	if cn.handsFreeUtt != nil {
		t.Error("commit left the utterance in flight")
	}
}

// Push-to-talk and calibration are each a one-request utterance.
func TestSingleClipCaptureKinds(t *testing.T) {
	for _, tc := range []struct {
		calibrate bool
		kind      string
		role      string
	}{
		{false, transcribe.CaptureFinal, "accurate"},
		{true, transcribe.CaptureCalibration, "fast"},
	} {
		stt := &recordingSTT{}
		cn := &conn{ctx: context.Background(), outDone: true,
			srv: &Server{stt: stt, fastStt: stt, tokens: testTokens(t), store: testStore(t)}}
		cn.startAudio(codecPCM16, false, tc.calibrate, "", nil)
		cn.handleAudioFrame(make([]byte, 3200))
		cn.endAudio()
		if len(stt.got) != 1 {
			t.Fatalf("%s: %d requests, want 1", tc.kind, len(stt.got))
		}
		g := stt.got[0]
		if g.Kind != tc.kind || g.Seq != 1 || g.UtteranceID == "" || g.HandsFree || g.Codec != codecPCM16 || g.STTRole != tc.role {
			t.Errorf("%s: capture = %+v", tc.kind, g)
		}
		if g.RecordedAt.IsZero() || time.Since(g.RecordedAt) > time.Minute {
			t.Errorf("%s: recorded_at = %v", tc.kind, g.RecordedAt)
		}
	}
}

// The session a clip targets is resolved from the store; with no target, the
// attached session is used. An empty Host is the local machine.
func TestCaptureSession(t *testing.T) {
	st := testStore(t)
	if err := st.Put(&session.Session{Name: "spawner", Dir: "/data/claude_spawner", SessionID: "sid-1"}); err != nil {
		t.Fatal(err)
	}
	cn := &conn{srv: &Server{store: st}}
	cp := cn.capture(&utterance{id: "u", seq: 1, sessionID: "sid-1"}, transcribe.CaptureFinal, "accurate", false)
	if cp.SessionID != "sid-1" || cp.SessionName != "spawner" || cp.SessionCwd != "/data/claude_spawner" || cp.Host != "local" {
		t.Errorf("targeted session capture = %+v", cp)
	}
	cn.attached = &session.Session{Name: "other", Dir: "/tmp/x", SessionID: "sid-2", Host: "work"}
	cp = cn.capture(&utterance{id: "u", seq: 1}, transcribe.CaptureFinal, "accurate", false)
	if cp.SessionID != "sid-2" || cp.SessionName != "other" || cp.Host != "work" {
		t.Errorf("attached-session capture = %+v", cp)
	}
}

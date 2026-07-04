package usage

import (
	"math"
	"path/filepath"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// TestDriftLearnAndCalibrate exercises the core loop: before any calibration the
// estimate is unknown; the first /usage anchors it; a second /usage over a known
// token/percent interval learns the rate; then turns drift the estimate up along
// that learned rate.
func TestDriftLearnAndCalibrate(t *testing.T) {
	e := Open("")

	if v := e.View(); v.Calibrated || v.SessionEstPct != -1 {
		t.Fatalf("fresh estimator should be uncalibrated with est −1, got %+v", v)
	}

	// First check: session at 40%, week at 20%. Anchors, no rate learned yet.
	e.AddTurn(100_000)
	e.Calibrate(1000, 40, 20)
	if v := e.View(); !v.Calibrated || !approx(v.SessionEstPct, 40) || !approx(v.SessionRealPct, 40) {
		t.Fatalf("after first calibrate expected est=real=40, got %+v", v)
	}

	// 200k tokens later, session reads 50% (10 pts) → learned rate 20k tokens/%.
	e.AddTurn(200_000)
	e.Calibrate(2000, 50, 25) // week +5 over 200k → 40k tokens/%
	v := e.View()
	if !approx(v.SessionEstPct, 50) || !approx(v.SessionRealPct, 50) {
		t.Fatalf("second calibrate should snap session to 50, got %+v", v)
	}
	if v.TokensSinceCheck != 0 || v.TurnsSinceCheck != 0 {
		t.Fatalf("calibrate should zero since-check counters, got %+v", v)
	}

	// Drift: 40k more tokens at the learned 20k/% → +2% session (52%).
	e.AddTurn(40_000)
	v = e.View()
	if !approx(v.SessionEstPct, 52) {
		t.Errorf("session est after drift = %.2f, want 52", v.SessionEstPct)
	}
	if !approx(v.WeekEstPct, 26) { // 25 + 40k/40k = +1
		t.Errorf("week est after drift = %.2f, want 26", v.WeekEstPct)
	}
	if v.TokensSinceCheck != 40_000 || v.TurnsSinceCheck != 1 {
		t.Errorf("since-check = %d tokens / %d turns, want 40000 / 1", v.TokensSinceCheck, v.TurnsSinceCheck)
	}
}

// TestSessionWindowReset checks that a forward jump in the session reset time
// (a rolled-over 5-hour window) drops the session anchor to 0 so the estimate
// restarts from the bottom, without touching the week window.
func TestSessionWindowReset(t *testing.T) {
	e := Open("")
	e.NoteSessionResetsAt(5000)
	e.AddTurn(100_000)
	e.Calibrate(1000, 80, 30)
	// Learn a session rate: +10% over 50k → 5k tokens/%.
	e.AddTurn(50_000)
	e.Calibrate(2000, 90, 35)

	if v := e.View(); !approx(v.SessionEstPct, 90) {
		t.Fatalf("pre-reset session est should be 90, got %.2f", v.SessionEstPct)
	}
	// Window rolls over: reset time jumps forward. Session anchor → 0; week intact.
	e.NoteSessionResetsAt(23000)
	v := e.View()
	if !approx(v.SessionEstPct, 0) {
		t.Errorf("after reset session est = %.2f, want 0", v.SessionEstPct)
	}
	if v.SessionRealPct != 0 {
		t.Errorf("after reset session real anchor = %.2f, want 0", v.SessionRealPct)
	}
	if v.WeekEstPct <= 0 {
		t.Errorf("week window should survive a session reset, got est %.2f", v.WeekEstPct)
	}
}

// TestPersistRoundTrip confirms the odometer and learned rates survive a reopen.
func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage_estimate.json")
	e := Open(path)
	e.AddTurn(100_000)
	e.Calibrate(1000, 40, 20)
	e.AddTurn(200_000)
	e.Calibrate(2000, 50, 25) // rate 20k/%
	before := e.View()

	e2 := Open(path)
	e2.AddTurn(40_000)
	after := e2.View()
	if !approx(after.SessionEstPct, 52) {
		t.Errorf("reopened estimator drift = %.2f, want 52 (rate must persist)", after.SessionEstPct)
	}
	if after.CumTokens != before.CumTokens+40_000 {
		t.Errorf("odometer not persisted: got %d, want %d", after.CumTokens, before.CumTokens+40_000)
	}
}

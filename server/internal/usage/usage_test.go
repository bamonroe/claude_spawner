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

// TestBenchmarkTwoPoint exercises the manual "set"/"calc" path: set marks the
// odometer + real percentages, a run burns tokens, and calc derives the rate
// DIRECTLY from the interval (no EMA) then re-anchors the estimate. A sub-1%
// move is refused so the rate is left untouched.
func TestBenchmarkTwoPoint(t *testing.T) {
	e := Open("")
	e.AddTurn(1_000_000)
	e.Calibrate(1000, 20, 40) // anchor; seeds still in place

	// "set" at session 20% / week 40%, marking the current odometer.
	if v := e.SetBenchmark(20, 40); !v.BenchSet || v.BenchTokens != 1_000_000 {
		t.Fatalf("set should stamp odometer at 1e6, got %+v", v)
	}

	// Burn 300k tokens, then "calc" at 25% session (+5) / 41% week (+1).
	e.AddTurn(300_000)
	v, sessOK, weekOK := e.CalcBenchmark(2000, 25, 41)
	if !sessOK || !weekOK {
		t.Fatalf("calc should update both windows (sess=%v week=%v)", sessOK, weekOK)
	}
	// Session rate = 300k / 5% = 60k/%; estimate snaps to the real 25%.
	if !approx(v.SessionEstPct, 25) {
		t.Errorf("calc should snap session est to 25, got %.2f", v.SessionEstPct)
	}
	e.AddTurn(60_000) // +1% at the freshly-set 60k/% session rate → 26%
	if got := e.View().SessionEstPct; !approx(got, 26) {
		t.Errorf("drift on calc'd rate = %.2f, want 26 (rate should be 60k/%%)", got)
	}

	// Too-small a move since a new benchmark leaves the rate untouched.
	e.SetBenchmark(26, 42)
	e.AddTurn(10_000)
	if _, sOK, wOK := e.CalcBenchmark(3000, 26, 42); sOK || wOK {
		t.Errorf("sub-1%% move should refuse to learn, got sess=%v week=%v", sOK, wOK)
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

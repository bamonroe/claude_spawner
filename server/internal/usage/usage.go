// Package usage maintains a server-global, drift-live estimate of Claude plan
// usage aggregated across ALL sessions and clients. Every turn adds its weighted
// token cost to a running odometer and nudges the estimated session/weekly
// percent-used upward; a `/usage` calibration snaps the estimate back to Claude's
// real numbers and refines the learned tokens-per-percent rate so the drift
// self-corrects over time. State is persisted so the odometer and learned rates
// survive restarts. All methods are safe for concurrent use.
package usage

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
)

const (
	// Seed tokens-per-percent, used only until two consecutive same-window
	// calibrations let us learn the real rate. Rough Max-plan ballparks; the first
	// /usage-to-/usage interval replaces them. A week's budget dwarfs a single
	// 5-hour session, so its seed is much larger.
	defaultSessRate = 40000.0
	defaultWeekRate = 600000.0
	// Rate learning: require at least this many percent gained between checks to
	// trust the derived rate (avoids noise and divide-by-tiny), then EMA-blend it
	// in for stability instead of jumping to each raw observation.
	minLearnDeltaPct = 1.0
	rateEMA          = 0.4
)

// Estimator is the persistent, concurrency-safe usage estimator.
type Estimator struct {
	path string
	mu   sync.Mutex
	st   state
}

// state is the persisted form. Tokens are the summed weighted per-turn totals;
// percents are 0..100; a window's rate is tokens-per-percent.
type state struct {
	CumTokens  int64 `json:"cum_tokens"`  // monotonic odometer across all turns/clients
	TurnsTotal int64 `json:"turns_total"` // total turns counted, ever

	Calibrated      bool  `json:"calibrated"`        // has a /usage anchor been set yet?
	LastCheckAt     int64 `json:"last_check_at"`     // unix seconds of the last /usage calibration
	LastCheckTokens int64 `json:"last_check_tokens"` // CumTokens at that calibration
	TurnsSinceCheck int64 `json:"turns_since_check"` // turns counted since the last calibration

	// Session window (5-hour rolling).
	SessAnchorPct    float64 `json:"sess_anchor_pct"`    // real % at last calibration (0 after a reset)
	SessAnchorTokens int64   `json:"sess_anchor_tokens"` // CumTokens at that anchor
	SessRate         float64 `json:"sess_rate"`          // tokens per percent (learned; seeded)
	SessLearned      bool    `json:"sess_learned"`       // has a real rate replaced the seed yet?
	SessResetsAt     int64   `json:"sess_resets_at"`     // unix; a jump forward means the window rolled over

	// Weekly window.
	WeekAnchorPct    float64 `json:"week_anchor_pct"`
	WeekAnchorTokens int64   `json:"week_anchor_tokens"`
	WeekRate         float64 `json:"week_rate"`
	WeekLearned      bool    `json:"week_learned"`

	// Manual two-point benchmark (the app's "set"/"calc" buttons). SetBenchmark
	// stamps the current odometer and real percentages here; CalcBenchmark later
	// derives each window's rate DIRECTLY from the interval to this mark, bypassing
	// the EMA damping and the single-interval rounding noise that bias the passive
	// /usage calibration low.
	BenchSet     bool    `json:"bench_set"`
	BenchTokens  int64   `json:"bench_tokens"`
	BenchSessPct float64 `json:"bench_sess_pct"`
	BenchWeekPct float64 `json:"bench_week_pct"`
}

// View is a snapshot for the wire/UI. The Est* percents are the current
// drift-estimated values (−1 until the first calibration provides an anchor);
// Real* are the last calibration's true numbers.
type View struct {
	Calibrated       bool
	SessionEstPct    float64
	WeekEstPct       float64
	SessionRealPct   float64
	WeekRealPct      float64
	CumTokens        int64
	TokensSinceCheck int64
	TurnsSinceCheck  int64
	LastCheckAt      int64

	// Manual benchmark ("set" button): whether one is armed and the percentages/
	// odometer it was stamped at, so the UI can show what "calc" will measure from.
	BenchSet       bool
	BenchSessPct   float64
	BenchWeekPct   float64
	BenchTokens    int64
	TokensSinceSet int64
}

// Open loads (or initializes) the estimator at path. A missing/corrupt file
// yields a fresh estimator with seeded rates. path may be "" (in-memory only).
func Open(path string) *Estimator {
	e := &Estimator{path: path, st: state{SessRate: defaultSessRate, WeekRate: defaultWeekRate}}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &e.st); err != nil {
				log.Printf("usage: state file %s is corrupt (%v); starting from seed rates", path, err)
			}
		} else if !os.IsNotExist(err) {
			log.Printf("usage: cannot read state file %s (%v); starting from seed rates", path, err)
		}
	}
	if e.st.SessRate <= 0 {
		e.st.SessRate = defaultSessRate
	}
	if e.st.WeekRate <= 0 {
		e.st.WeekRate = defaultWeekRate
	}
	return e
}

// AddTurn records one turn's weighted token cost (from any session/client),
// advancing the odometer so the estimate drifts up. Returns the updated view.
func (e *Estimator) AddTurn(tokens int64) View {
	if tokens < 0 {
		tokens = 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.CumTokens += tokens
	e.st.TurnsTotal++
	e.st.TurnsSinceCheck++
	e.persist()
	return e.viewLocked()
}

// NoteSessionResetsAt feeds the 5-hour window's reset time (from the stream-json
// rate_limit_event, which fires every turn). When it jumps forward the window has
// rolled over, so the session anchor drops to 0 and the estimate climbs from
// there — even without a /usage call in between.
func (e *Estimator) NoteSessionResetsAt(resetsAt int64) {
	if resetsAt <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case e.st.SessResetsAt == 0:
		e.st.SessResetsAt = resetsAt
	case resetsAt > e.st.SessResetsAt+60: // rolled over (allow small jitter)
		e.st.SessResetsAt = resetsAt
		e.st.SessAnchorPct = 0
		e.st.SessAnchorTokens = e.st.CumTokens
	default:
		return // unchanged; nothing to persist
	}
	e.persist()
}

// Calibrate snaps the estimate to Claude's real percentages (from /usage) and
// refines each window's tokens-per-percent rate from the interval since the last
// calibration. A percent that went DOWN means the window reset in between, so we
// re-anchor without learning from it. A negative pct means "unparsed" and leaves
// that window's anchor untouched. Returns the post-calibration view.
func (e *Estimator) Calibrate(nowUnix int64, sessionPct, weekPct float64) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st.Calibrated {
		learn(&e.st.SessRate, &e.st.SessLearned, e.st.SessAnchorPct, e.st.SessAnchorTokens, sessionPct, e.st.CumTokens)
		learn(&e.st.WeekRate, &e.st.WeekLearned, e.st.WeekAnchorPct, e.st.WeekAnchorTokens, weekPct, e.st.CumTokens)
	}
	if sessionPct >= 0 {
		e.st.SessAnchorPct = sessionPct
		e.st.SessAnchorTokens = e.st.CumTokens
	}
	if weekPct >= 0 {
		e.st.WeekAnchorPct = weekPct
		e.st.WeekAnchorTokens = e.st.CumTokens
	}
	e.st.Calibrated = true
	e.st.LastCheckAt = nowUnix
	e.st.LastCheckTokens = e.st.CumTokens
	e.st.TurnsSinceCheck = 0
	e.persist()
	return e.viewLocked()
}

// SetBenchmark stamps the current odometer position and Claude's real
// percentages as the start of a manual two-point rate measurement (the app's
// "set" button). A later CalcBenchmark derives tokens-per-percent from the
// interval to this mark. Returns the updated view.
func (e *Estimator) SetBenchmark(sessionPct, weekPct float64) View {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.BenchSet = true
	e.st.BenchTokens = e.st.CumTokens
	e.st.BenchSessPct = sessionPct
	e.st.BenchWeekPct = weekPct
	e.persist()
	return e.viewLocked()
}

// CalcBenchmark derives each window's tokens-per-percent DIRECTLY from the
// interval since SetBenchmark: rate = tokensSinceMark / percentGained. Unlike
// Calibrate it does not EMA-blend — the whole point is to snap the learned rate
// to one clean, deliberately large measurement, so a run big enough to move
// several whole percent drowns out the integer-rounding of the reported percent.
// A window whose percent gained is below minLearnDeltaPct (too small to divide
// reliably, or the window reset in between) is left untouched and reported as
// not-updated. It also re-anchors the drift estimate to the fresh real numbers,
// exactly like a /usage calibration. Returns the post-calc view plus whether the
// session/weekly rate was actually updated.
func (e *Estimator) CalcBenchmark(nowUnix int64, sessionPct, weekPct float64) (v View, sessOK, weekOK bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.st.BenchSet {
		dTok := float64(e.st.CumTokens - e.st.BenchTokens)
		if dTok > 0 {
			if sessionPct >= 0 {
				if d := sessionPct - e.st.BenchSessPct; d >= minLearnDeltaPct {
					e.st.SessRate = dTok / d
					e.st.SessLearned = true
					sessOK = true
				}
			}
			if weekPct >= 0 {
				if d := weekPct - e.st.BenchWeekPct; d >= minLearnDeltaPct {
					e.st.WeekRate = dTok / d
					e.st.WeekLearned = true
					weekOK = true
				}
			}
		}
	}
	// Re-anchor the drift estimate to the real numbers, same as Calibrate's snap.
	if sessionPct >= 0 {
		e.st.SessAnchorPct = sessionPct
		e.st.SessAnchorTokens = e.st.CumTokens
	}
	if weekPct >= 0 {
		e.st.WeekAnchorPct = weekPct
		e.st.WeekAnchorTokens = e.st.CumTokens
	}
	e.st.Calibrated = true
	e.st.LastCheckAt = nowUnix
	e.st.LastCheckTokens = e.st.CumTokens
	e.st.TurnsSinceCheck = 0
	e.persist()
	return e.viewLocked(), sessOK, weekOK
}

// View returns the current estimate snapshot.
func (e *Estimator) View() View {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.viewLocked()
}

// learn refines *rate (tokens/%) from the token/percent delta since an anchor.
// The first real observation REPLACES the rough seed outright; later ones EMA-
// blend in for stability. Skips window resets (realPct below the anchor) and
// sub-threshold deltas that would make the derived rate unreliable.
func learn(rate *float64, learned *bool, anchorPct float64, anchorTokens int64, realPct float64, cumTokens int64) {
	if realPct < 0 {
		return
	}
	dPct := realPct - anchorPct
	dTok := float64(cumTokens - anchorTokens)
	if dPct < minLearnDeltaPct || dTok <= 0 {
		return
	}
	observed := dTok / dPct
	if !*learned || *rate <= 0 {
		*rate = observed
		*learned = true
		return
	}
	*rate = rateEMA*observed + (1-rateEMA)*(*rate)
}

func (e *Estimator) viewLocked() View {
	v := View{
		Calibrated:      e.st.Calibrated,
		SessionEstPct:   -1,
		WeekEstPct:      -1,
		SessionRealPct:  -1,
		WeekRealPct:     -1,
		CumTokens:       e.st.CumTokens,
		TurnsSinceCheck: e.st.TurnsSinceCheck,
		LastCheckAt:     e.st.LastCheckAt,
		BenchSet:        e.st.BenchSet,
		BenchSessPct:    e.st.BenchSessPct,
		BenchWeekPct:    e.st.BenchWeekPct,
		BenchTokens:     e.st.BenchTokens,
	}
	if e.st.BenchSet {
		v.TokensSinceSet = e.st.CumTokens - e.st.BenchTokens
	}
	if e.st.Calibrated {
		v.SessionRealPct = e.st.SessAnchorPct
		v.WeekRealPct = e.st.WeekAnchorPct
		v.SessionEstPct = clampPct(e.st.SessAnchorPct + float64(e.st.CumTokens-e.st.SessAnchorTokens)/e.st.SessRate)
		v.WeekEstPct = clampPct(e.st.WeekAnchorPct + float64(e.st.CumTokens-e.st.WeekAnchorTokens)/e.st.WeekRate)
		v.TokensSinceCheck = e.st.CumTokens - e.st.LastCheckTokens
	}
	return v
}

func clampPct(p float64) float64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}

// persist atomically writes the state (temp file + rename). Caller holds e.mu.
func (e *Estimator) persist() {
	if e.path == "" {
		return
	}
	data, err := json.MarshalIndent(e.st, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(e.path), 0o755); err != nil {
		log.Printf("usage: cannot create state dir for %s (%v); estimate not persisted", e.path, err)
		return
	}
	tmp := e.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("usage: cannot write state to %s (%v); estimate not persisted", tmp, err)
		return
	}
	if err := os.Rename(tmp, e.path); err != nil {
		log.Printf("usage: cannot commit state to %s (%v); estimate not persisted", e.path, err)
	}
}

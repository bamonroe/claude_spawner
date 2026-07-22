// Package bgjob owns the spawner-side detached background-job mechanism: the
// embedded `spawner-job` POSIX-sh wrapper Claude invokes (via Bash) to launch a
// command fully detached from the turn, and the small helpers the server uses to
// stage that script onto a target and address its on-disk registry.
//
// Why this exists: each turn drives a fresh headless `claude` (resumed by
// session_id) whose process-group and pipes are torn down when the turn ends, and
// claude tracks its own run_in_background shells in-memory per process — so a bg
// job started with run_in_background dies at turn teardown and the next turn's
// fresh claude can't poll it. `spawner-job start` instead launches the command in
// its own setsid session with stdin from /dev/null and stdout/stderr to a log
// file, so it escapes the turn's pgid entirely and survives; a later turn's
// `spawner-job list --json` reports which jobs finished. The registry is keyed by
// the ABSOLUTE working directory (stable across session_id rotation), so a
// clear/compress doesn't lose track of jobs. See docs/architecture.md.
package bgjob

import (
	_ "embed"
	"encoding/json"
)

// Script is the embedded spawner-job wrapper, staged verbatim onto each target.
//
//go:embed spawner-job.sh
var Script string

// StagedName is the filename the script is written under on a target.
const StagedName = "spawner-job"

// Record is one background job as reported by `spawner-job list --json`. Done and
// Exit are derived at read time (from `kill -0 <pid>`); Exit is best-effort (a
// detached job's exit code isn't recoverable, so it's reported as 0 and the log
// tail carries the real outcome).
type Record struct {
	ID      string `json:"id"`
	PID     int    `json:"pid"`
	Cmd     string `json:"cmd"`
	Started int64  `json:"started"`
	Done    bool   `json:"done"`
	Exit    int    `json:"exit"`
	// Session is the session_id that launched the job (stamped by `start`). Empty
	// for a job started before this field existed — such jobs stay dir-attributed.
	Session string `json:"session"`
}

// ParseList unmarshals the JSON array printed by `spawner-job list --json`.
func ParseList(out []byte) ([]Record, error) {
	var recs []Record
	if err := json.Unmarshal(out, &recs); err != nil {
		return nil, err
	}
	return recs, nil
}

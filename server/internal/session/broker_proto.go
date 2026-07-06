package session

// Wire protocol for the host-side broker. The broker lets a (containerized,
// unprivileged) spawner run turns on the host without holding host root: the
// server dials a Unix socket and the broker — running as the ordinary host user —
// enforces the SPAWNER_ROOT jail and executes on its behalf. It is the single
// host-side execution agent for BOTH targets: it forks claude for "host" turns
// and drives the rootless container runtime for "sandbox" turns (create/exec/
// remove), so the server container needs neither host root nor a runtime socket.
// See docs/architecture.md.
//
// A request is one framed header (a brokerRequest). For an OpTurn the broker
// replies with zero or more FrameStdout frames then a FrameExit; for the unary
// ops (ensure/remove/list) it replies with one FrameResult.

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	frameHeader byte = 'h' // client→broker: brokerRequest
	frameStdout byte = 'o' // broker→client: a chunk of a turn's stdout
	frameExit   byte = 'e' // broker→client: brokerExit (turn finished/failed)
	frameResult byte = 'r' // broker→client: brokerResult (unary op outcome)
)

// maxFrame bounds a single frame payload (16 MB) — matches the stream-json line
// cap so no legitimate frame is rejected while a corrupt length can't make the
// reader allocate unboundedly.
const maxFrame = 16 << 20

// brokerOp names the action a brokerRequest asks the host-side broker to perform.
type brokerOp string

const (
	opTurn    brokerOp = "turn"    // run a turn (streamed): fork claude or exec into the sandbox
	opEnsure  brokerOp = "ensure"  // create/start a session's sandbox container
	opRemove  brokerOp = "remove"  // delete a session's sandbox container
	opList    brokerOp = "list"    // list managed sandbox containers (for reconcile)
	opRestart brokerOp = "restart" // rebuild + relaunch the (containerized) server on the host
	opDelete  brokerOp = "delete"  // permanently remove a directory's Claude transcripts (host-side)
	opMkdir   brokerOp = "mkdir"   // create a new (jailed) project directory host-side
)

// brokerRequest is the client→broker header. Target/Container are set for turns
// (and Container for ensure/remove); Args is the claude command for a turn.
type brokerRequest struct {
	Op        brokerOp `json:"op"`
	Target    string   `json:"target,omitempty"`
	Dir       string   `json:"dir,omitempty"`
	Args      []string `json:"args,omitempty"`
	Container string   `json:"container,omitempty"`
	SessionID string   `json:"session_id,omitempty"` // opDelete (legacy whole-dir): any session known to live in Dir
	IDs       []string `json:"ids,omitempty"`        // opDelete (per-session): exact session_ids to remove; when set, Dir/SessionID are ignored
}

// brokerExit is the turn trailer: claude's exit status (0 = success); Err carries
// a broker-side failure (jail rejection, launch error).
type brokerExit struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
}

// brokerResult is the reply to a unary op: Err on failure, Names for a list.
type brokerResult struct {
	Err   string   `json:"err,omitempty"`
	Names []string `json:"names,omitempty"`
}

// writeFrame writes one length-prefixed frame: a type byte, a 4-byte big-endian
// payload length, then the payload.
func writeFrame(w io.Writer, typ byte, payload []byte) error {
	var hdr [5]byte
	hdr[0] = typ
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	return nil
}

// readFrame reads one frame written by writeFrame. It returns io.EOF cleanly when
// the stream ends on a frame boundary.
func readFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("broker frame too large: %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	return hdr[0], buf, nil
}

// Package broker defines the wire protocol and host-side daemon that lets a
// (containerized, unprivileged) spawner run a claude turn directly on the host
// without the server holding host root. The server dials a Unix socket and sends
// a Request; the broker — running as the ordinary host user — validates the
// working directory against its own SPAWNER_ROOT jail, forks claude, and streams
// stdout back. The server can only ever ask for this one constrained action, so a
// compromised server container cannot run arbitrary host commands; the broker is
// the authoritative place the jail is enforced. See docs/architecture.md.
package broker

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Frame types on the socket. The client sends exactly one FrameHeader; the broker
// replies with zero or more FrameStdout frames followed by exactly one FrameExit.
const (
	FrameHeader byte = 'h' // client→broker: JSON Request (the turn to run)
	FrameStdout byte = 'o' // broker→client: a chunk of claude's stdout
	FrameExit   byte = 'e' // broker→client: JSON Exit (turn finished / failed)
)

// maxFrame bounds a single frame payload (16 MB) — matches the stream-json line
// cap in the session parser, so no legitimate frame is rejected while a corrupt
// length can't make the reader allocate unboundedly.
const maxFrame = 16 << 20

// Request is the client→broker header: run `claude <Args...>` in Dir. Dir is
// validated against the broker's jail before anything launches.
type Request struct {
	Dir  string   `json:"dir"`
	Args []string `json:"args"`
}

// Exit is the broker→client trailer: the turn's outcome. Code is claude's exit
// status (0 = success); Err carries a broker-side failure (jail rejection, launch
// error) that happened before/around the process.
type Exit struct {
	Code int    `json:"code"`
	Err  string `json:"err,omitempty"`
}

// WriteFrame writes one length-prefixed frame: 1 type byte, a 4-byte big-endian
// payload length, then the payload.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
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

// ReadFrame reads one frame written by WriteFrame. It returns io.EOF cleanly when
// the stream ends on a frame boundary.
func ReadFrame(r io.Reader) (typ byte, payload []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err // io.EOF at a clean boundary; ErrUnexpectedEOF mid-header
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

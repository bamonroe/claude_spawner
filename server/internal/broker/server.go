package broker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os/exec"
	"syscall"
)

// Server is the host-side broker. Validate confines a requested working directory
// to the jail (typically config.ValidateSpawnDir) and returns the cleaned path;
// ClaudeBin is the claude binary the broker forks (the real host claude). The
// broker never runs any other program, so a client can only ask it to launch
// claude in a jailed directory — never an arbitrary host command.
type Server struct {
	Validate  func(dir string) (string, error)
	ClaudeBin string
	// Logf logs accept/handle errors; nil uses the standard logger.
	Logf func(format string, args ...any)
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Serve accepts connections until l is closed, handling each in its own
// goroutine. It returns the listener error (nil on a clean Close).
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(conn)
	}
}

// handle runs one turn: read the Request header, jail-check its dir, fork claude,
// stream stdout back as frames, and send an Exit trailer. If the client closes
// the connection mid-turn (an abort), the process group is killed.
func (s *Server) handle(conn net.Conn) {
	defer conn.Close()

	typ, payload, err := ReadFrame(conn)
	if err != nil || typ != FrameHeader {
		s.sendExit(conn, -1, "expected header frame")
		return
	}
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		s.sendExit(conn, -1, "bad request: "+err.Error())
		return
	}
	dir, err := s.Validate(req.Dir)
	if err != nil {
		s.logf("broker: rejected dir %q: %v", req.Dir, err)
		s.sendExit(conn, -1, "jail: "+err.Error())
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, s.ClaudeBin, req.Args...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		s.sendExit(conn, -1, "stdout pipe: "+err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		s.sendExit(conn, -1, "start claude: "+err.Error())
		return
	}

	// The client sends nothing after the header, so any read completing means it
	// closed the socket — treat that as an abort and cancel the turn.
	go func() {
		var b [1]byte
		_, _ = conn.Read(b[:])
		cancel()
	}()

	buf := make([]byte, 32<<10)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			if werr := WriteFrame(conn, FrameStdout, buf[:n]); werr != nil {
				cancel() // client gone; stop the turn
				break
			}
		}
		if rerr != nil {
			break
		}
	}

	code, errStr := 0, ""
	if werr := cmd.Wait(); werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			code, errStr = -1, werr.Error()
		}
	}
	s.sendExit(conn, code, errStr)
}

func (s *Server) sendExit(conn net.Conn, code int, errStr string) {
	b, _ := json.Marshal(Exit{Code: code, Err: errStr})
	if err := WriteFrame(conn, FrameExit, b); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.logf("broker: send exit: %v", err)
	}
}

package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"os/exec"
)

// BrokerServer is the host-side broker daemon. It runs on the host as the
// ordinary user and executes turns on behalf of a (containerized, unprivileged)
// spawner: it forks claude for "host" turns via Host and drives the container
// runtime for "sandbox" turns via Sandbox — the SAME executor code the server
// uses when it runs natively, so there's one implementation. Validate confines a
// requested working directory to the jail (typically config.ValidateSpawnDir).
// The server can only ask for these constrained actions, never an arbitrary host
// command, and this is where the jail is authoritatively enforced.
type BrokerServer struct {
	Validate   func(dir string) (string, error)
	Host       HostExecutor
	Sandbox    SandboxExecutor
	HasSandbox bool // Sandbox is configured (sandbox-target requests are accepted)
	Logf       func(format string, args ...any)
}

func (s *BrokerServer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// Serve accepts connections until l is closed, handling each in its own
// goroutine. It returns the listener error (nil on a clean Close).
func (s *BrokerServer) Serve(l net.Listener) error {
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

func (s *BrokerServer) handle(conn net.Conn) {
	defer conn.Close()
	typ, payload, err := readFrame(conn)
	if err != nil || typ != frameHeader {
		s.sendResult(conn, brokerResult{Err: "expected header frame"})
		return
	}
	var req brokerRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		s.sendResult(conn, brokerResult{Err: "bad request: " + err.Error()})
		return
	}
	switch req.Op {
	case opTurn, "":
		s.handleTurn(conn, req)
	case opEnsure:
		s.sendResult(conn, brokerResult{Err: errStr(s.ensure(req))})
	case opRemove:
		s.sendResult(conn, brokerResult{Err: errStr(s.remove(req))})
	case opList:
		names, err := s.list()
		s.sendResult(conn, brokerResult{Names: names, Err: errStr(err)})
	default:
		s.sendResult(conn, brokerResult{Err: "unknown op " + string(req.Op)})
	}
}

// handleTurn runs one turn through the target's executor and streams stdout back
// as frames, then an exit trailer. A client disconnect mid-turn aborts it (the
// executor kills the process group / runtime client).
func (s *BrokerServer) handleTurn(conn net.Conn, req brokerRequest) {
	dir, err := s.Validate(req.Dir)
	if err != nil {
		s.logf("broker: rejected dir %q: %v", req.Dir, err)
		s.sendExit(conn, -1, "jail: "+err.Error())
		return
	}
	ex, err := s.turnExecutor(Target(req.Target))
	if err != nil {
		s.sendExit(conn, -1, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess := &Session{Name: "broker", Dir: dir, Container: req.Container, Target: Target(req.Target)}
	proc, err := ex.Start(ctx, sess, req.Args)
	if err != nil {
		s.sendExit(conn, -1, err.Error())
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
		n, rerr := proc.Stdout().Read(buf)
		if n > 0 {
			if werr := writeFrame(conn, frameStdout, buf[:n]); werr != nil {
				cancel()
				break
			}
		}
		if rerr != nil {
			break
		}
	}
	code, errMsg := 0, ""
	if werr := proc.Wait(); werr != nil {
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else {
			code, errMsg = -1, werr.Error()
		}
	}
	s.sendExit(conn, code, errMsg)
}

// turnExecutor picks the executor for a turn's target: sandbox when requested and
// configured, otherwise the host fork.
func (s *BrokerServer) turnExecutor(t Target) (Executor, error) {
	if t == TargetSandbox {
		if !s.HasSandbox {
			return nil, errors.New("sandbox target requested but the broker has no sandbox configured")
		}
		return s.Sandbox, nil
	}
	return s.Host, nil
}

func (s *BrokerServer) ensure(req brokerRequest) error {
	if !s.HasSandbox {
		return errors.New("no sandbox configured")
	}
	dir, err := s.Validate(req.Dir)
	if err != nil {
		return err
	}
	return s.Sandbox.Ensure(context.Background(), req.Container, dir)
}

func (s *BrokerServer) remove(req brokerRequest) error {
	if !s.HasSandbox {
		return errors.New("no sandbox configured")
	}
	return s.Sandbox.Remove(context.Background(), req.Container)
}

func (s *BrokerServer) list() ([]string, error) {
	if !s.HasSandbox {
		return nil, nil
	}
	return s.Sandbox.List(context.Background())
}

func (s *BrokerServer) sendExit(conn net.Conn, code int, errMsg string) {
	b, _ := json.Marshal(brokerExit{Code: code, Err: errMsg})
	if err := writeFrame(conn, frameExit, b); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.logf("broker: send exit: %v", err)
	}
}

func (s *BrokerServer) sendResult(conn net.Conn, res brokerResult) {
	b, _ := json.Marshal(res)
	if err := writeFrame(conn, frameResult, b); err != nil && !errors.Is(err, io.ErrClosedPipe) {
		s.logf("broker: send result: %v", err)
	}
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

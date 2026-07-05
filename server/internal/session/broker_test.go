package session

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeClaude writes a shell script that emits a stream-json result event
// (ignoring its args) and returns its path.
func writeFakeClaude(t *testing.T, result string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"" + result + "\"}'\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// startBroker runs a host-only BrokerServer on a temp unix socket and returns its
// path. startBrokerSrv is the general form (with a sandbox executor).
func startBroker(t *testing.T, validate func(string) (string, error), claudeBin string) string {
	t.Helper()
	return startBrokerSrv(t, &BrokerServer{Validate: validate, Host: HostExecutor{Bin: claudeBin}, Logf: t.Logf})
}

func startBrokerSrv(t *testing.T, srv *BrokerServer) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "broker.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(l)
	t.Cleanup(func() { l.Close() })
	return sock
}

func TestBrokerExecutorRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sock := startBroker(t, func(d string) (string, error) { return d, nil }, writeFakeClaude(t, "pong"))
	d := &Driver{Execs: map[Target]Executor{TargetHost: BrokerExecutor{Socket: sock}}, Bypass: true}

	s := &Session{Name: "s", Dir: dir, SessionID: "sid"}
	reply, _, err := d.Turn(context.Background(), s, "hi", nil, nil, nil)
	if err != nil {
		t.Fatalf("Turn via broker: %v", err)
	}
	if reply != "pong" {
		t.Errorf("reply = %q, want pong", reply)
	}
}

func TestBrokerExecutorJailRejection(t *testing.T) {
	sock := startBroker(t,
		func(string) (string, error) { return "", os.ErrPermission },
		writeFakeClaude(t, "pong"))
	d := &Driver{Execs: map[Target]Executor{TargetHost: BrokerExecutor{Socket: sock}}, Bypass: true}

	s := &Session{Name: "s", Dir: "/etc", SessionID: "sid"}
	_, _, err := d.Turn(context.Background(), s, "hi", nil, nil, nil)
	if err == nil {
		t.Fatal("expected a jail rejection error, got nil")
	}
	if !strings.Contains(err.Error(), "jail") {
		t.Errorf("error %q should mention the jail rejection", err)
	}
}

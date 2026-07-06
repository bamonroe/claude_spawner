package session

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestBrokerRestart(t *testing.T) {
	// With no RestartCmd configured, restart is refused with a clear error.
	sock := startBroker(t, func(d string) (string, error) { return d, nil }, "claude")
	client := BrokerExecutor{Socket: sock}
	if err := client.Restart(context.Background()); err == nil {
		t.Fatal("expected an error when restart is not configured")
	} else if !strings.Contains(err.Error(), "not configured") {
		t.Errorf("error %q should say restart is not configured", err)
	}

	// With both commands set, the broker launches each; two marker files prove
	// RestartCmd (server rebuild) and RestartSelfCmd (broker self-restart) both ran.
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	selfMarker := filepath.Join(dir, "self-restarted")
	sock2 := startBrokerSrv(t, &BrokerServer{
		Validate:       func(d string) (string, error) { return d, nil },
		Host:           HostExecutor{Bin: "claude"},
		RestartCmd:     "touch " + marker,
		RestartSelfCmd: "touch " + selfMarker,
		Logf:           t.Logf,
	})
	client2 := BrokerExecutor{Socket: sock2}
	if err := client2.Restart(context.Background()); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	// Both commands run detached; poll briefly for their side effects.
	for i := 0; i < 100; i++ {
		_, e1 := os.Stat(marker)
		_, e2 := os.Stat(selfMarker)
		if e1 == nil && e2 == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("restart commands did not both run (marker files never appeared)")
}

func TestBrokerDeleteSessions(t *testing.T) {
	// The broker runs as the host user that owns ~/.claude, so a delete op there
	// removes transcripts the read-only server container can't. Point HOME at a
	// temp dir and plant a transcript for a known cwd.
	home := t.TempDir()
	t.Setenv("HOME", home)
	cwd := "/home/bam/git/proj"
	proj := filepath.Join(home, ".claude", "projects", "enc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(proj, "sid.jsonl")
	line := `{"cwd":"` + cwd + `","type":"user"}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	sock := startBroker(t, func(d string) (string, error) { return d, nil }, "claude")
	client := BrokerExecutor{Socket: sock}
	if err := client.DeleteSessions(context.Background(), "sid", cwd); err != nil {
		t.Fatalf("DeleteSessions: %v", err)
	}
	if _, err := os.Stat(transcript); !os.IsNotExist(err) {
		t.Errorf("transcript still present after delete (stat err = %v)", err)
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

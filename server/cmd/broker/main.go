// Command broker is the host-side session broker. It runs on the host as the
// ordinary user (NOT root) and executes turns on behalf of a containerized,
// unprivileged spawner: the server dials the Unix socket and the broker enforces
// the SPAWNER_ROOT jail and runs the turn — forking claude for a "host" turn, or
// driving the rootless container runtime for a "sandbox" turn (create/exec/remove
// of the session's container). The server never gains the ability to run
// arbitrary host commands. See internal/session (broker_server.go) and
// docs/architecture.md.
//
// Env: SPAWNER_BROKER_SOCKET (required, the Unix socket path to listen on),
// SPAWNER_ROOT (the spawn jail, shared with the server), SPAWNER_CLAUDE_BIN
// (host claude binary), SPAWNER_BROKER_RESTART_CMD (shell command that rebuilds +
// relaunches the server container for the "restart" button; empty disables it).
// Sandbox turns additionally read the same SPAWNER_SANDBOX_* vars the server uses
// (IMAGE enables sandbox; RUNTIME, CLAUDE_BIN, MOUNTS, RUN_ARGS) — here the
// broker, not the server, owns the runtime config.
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/session"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// splitList splits a comma-separated value, trimming and dropping empties.
func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func main() {
	sockPath := os.Getenv("SPAWNER_BROKER_SOCKET")
	if sockPath == "" {
		log.Fatal("SPAWNER_BROKER_SOCKET is required (the Unix socket to listen on)")
	}
	roots, err := config.ParseRoots(os.Getenv("SPAWNER_ROOT"))
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if len(roots) == 0 {
		log.Printf("WARNING: SPAWNER_ROOT is empty — the broker will run turns in ANY " +
			"directory the server asks for (no jail). Set SPAWNER_ROOT to constrain it.")
	}
	cfg := &config.Config{SpawnRoots: roots}

	srv := &session.BrokerServer{
		Validate:   cfg.ValidateSpawnDir,
		Host:       session.HostExecutor{Bin: env("SPAWNER_CLAUDE_BIN", "claude")},
		RestartCmd: os.Getenv("SPAWNER_BROKER_RESTART_CMD"),
		Logf:       log.Printf,
	}
	if img := os.Getenv("SPAWNER_SANDBOX_IMAGE"); img != "" {
		srv.Sandbox = session.SandboxExecutor{
			Runtime: env("SPAWNER_SANDBOX_RUNTIME", "podman"),
			Image:   img,
			Bin:     env("SPAWNER_SANDBOX_CLAUDE_BIN", "claude"),
			Mounts:  splitList(os.Getenv("SPAWNER_SANDBOX_MOUNTS")),
			RunArgs: strings.Fields(os.Getenv("SPAWNER_SANDBOX_RUN_ARGS")),
		}
		srv.HasSandbox = true
	}

	// Fresh socket: remove a stale file from a previous run, then restrict access
	// to the owner (only local processes that can reach the socket may drive it).
	_ = os.Remove(sockPath)
	old := syscall.Umask(0o177)
	l, err := net.Listen("unix", sockPath)
	syscall.Umask(old)
	if err != nil {
		log.Fatalf("listen %s: %v", sockPath, err)
	}
	defer os.Remove(sockPath)

	log.Printf("broker listening on %s (claude=%q, sandbox=%v, roots=%v)",
		sockPath, srv.Host.Bin, srv.HasSandbox, roots)

	// Close the listener on SIGINT/SIGTERM so Serve returns and the socket file is
	// cleaned up.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		l.Close()
	}()

	if err := srv.Serve(l); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

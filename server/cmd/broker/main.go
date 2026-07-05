// Command broker is the host-side session broker. It runs on the host as the
// ordinary user (NOT root) and lets a containerized, unprivileged spawner run
// claude turns directly on the host: the server dials the Unix socket and asks
// the broker to launch claude in a directory; the broker enforces the SPAWNER_ROOT
// jail and forks claude as the user. The server never gains the ability to run
// arbitrary host commands — only this one constrained action. See internal/broker
// and docs/architecture.md.
//
// Env: SPAWNER_BROKER_SOCKET (required, the Unix socket path to listen on),
// SPAWNER_ROOT (the spawn jail, shared with the server), SPAWNER_CLAUDE_BIN
// (the claude binary, default "claude").
package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/bam/claude_spawner/server/internal/broker"
	"github.com/bam/claude_spawner/server/internal/config"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
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
		log.Printf("WARNING: SPAWNER_ROOT is empty — the broker will launch claude in ANY " +
			"directory the server asks for (no jail). Set SPAWNER_ROOT to constrain it.")
	}
	cfg := &config.Config{SpawnRoots: roots}

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

	srv := &broker.Server{Validate: cfg.ValidateSpawnDir, ClaudeBin: env("SPAWNER_CLAUDE_BIN", "claude")}
	log.Printf("broker listening on %s (claude=%q, roots=%v)", sockPath, srv.ClaudeBin, roots)

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

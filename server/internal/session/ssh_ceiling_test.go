package session

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveSSHChannelCeiling reports the host's real concurrent-channel limit on
// one connection, which is what sshMaxChannels/sshMaxStreamChannels are budgeted
// under. Gated on SPAWNER_SSH_LIVE=1 (it opens channels until the peer refuses,
// so it must not run against a host doing real work). Run:
//
//	SPAWNER_SSH_LIVE=1 go test ./internal/session/ -run TestLiveSSHChannelCeiling -v
//
// Record the number it prints in docs/architecture.md next to the pool
// description; it fails only if the ceiling is below our budget.
func TestLiveSSHChannelCeiling(t *testing.T) {
	if os.Getenv("SPAWNER_SSH_LIVE") != "1" {
		t.Skip("set SPAWNER_SSH_LIVE=1 to run the live channel-ceiling probe")
	}
	host := os.Getenv("SPAWNER_SSH_LIVE_HOST")
	if host == "" {
		host = "localhost"
	}
	// Auth comes from the environment so the probe can run against loopback
	// without the server's host registry: SPAWNER_SSH_LIVE_KEY names a private
	// key authorized on the target, SPAWNER_SSH_LIVE_USER its login.
	pool, err := NewSSHPool(SSHConfig{
		KeyFile: os.Getenv("SPAWNER_SSH_LIVE_KEY"),
		User:    os.Getenv("SPAWNER_SSH_LIVE_USER"),
	}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	n, err := pool.ProbeChannelCeiling(ctx, host)
	t.Logf("%s granted %d concurrent channels on one connection (ended with: %v)", host, n, err)
	if n < sshMaxChannels {
		t.Errorf("channel ceiling %d is below sshMaxChannels %d — the budget over-commits the connection", n, sshMaxChannels)
	}
}

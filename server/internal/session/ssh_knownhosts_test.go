package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// testHostKey returns a throwaway ed25519 SSH public key for known_hosts tests.
func testHostKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

// TestForgetHostRemovesOnlyItsRecord: ForgetHost drops the target host's lines and
// leaves the rest, then reloads verification — the "delete old records" path.
func TestForgetHostRemovesOnlyItsRecord(t *testing.T) {
	key := testHostKey(t)
	kh := filepath.Join(t.TempDir(), "known_hosts")
	lineA := knownhosts.Line([]string{"hosta"}, key)
	lineB := knownhosts.Line([]string{"hostb"}, key)
	if err := os.WriteFile(kh, []byte(lineA+"\n"+lineB+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	pool, err := NewSSHPool(SSHConfig{KnownHosts: kh}, nil, nil)
	if err != nil {
		t.Fatalf("NewSSHPool: %v", err)
	}
	if err := pool.ForgetHost("hosta", 0); err != nil {
		t.Fatalf("ForgetHost: %v", err)
	}
	data, err := os.ReadFile(kh)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hosta ") {
		t.Fatalf("hosta record was not removed:\n%s", data)
	}
	if !strings.Contains(string(data), "hostb ") {
		t.Fatalf("hostb record was wrongly removed:\n%s", data)
	}
	// Forgetting a host with no record is a harmless no-op.
	if err := pool.ForgetHost("nope", 0); err != nil {
		t.Fatalf("ForgetHost(nonexistent): %v", err)
	}
}

func TestKnownHostsLineMatches(t *testing.T) {
	key := testHostKey(t)
	plain := knownhosts.Line([]string{"example.com"}, key)
	if !knownHostsLineMatches(plain, "example.com") {
		t.Fatal("plaintext host should match")
	}
	if knownHostsLineMatches(plain, "other.com") {
		t.Fatal("non-matching host should not match")
	}
	ported := knownhosts.Line([]string{"[example.com]:2222"}, key)
	if !knownHostsLineMatches(ported, "[example.com]:2222") {
		t.Fatal("[host]:port entry should match")
	}
}

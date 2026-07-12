package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureServerKey: first call mints the keypair (private 0600 + .pub authorized
// line); a second call reads the existing key and returns the SAME public line, so a
// container recreate reuses the identity rather than churning it.
func TestEnsureServerKey(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), "sub", "id_ed25519")

	pub1, err := EnsureServerKey(keyPath)
	if err != nil {
		t.Fatalf("first EnsureServerKey: %v", err)
	}
	if !strings.HasPrefix(pub1, "ssh-ed25519 ") {
		t.Errorf("public line = %q, want ssh-ed25519 prefix", pub1)
	}

	// Private key exists with 0600, and the .pub matches the returned line.
	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat private key: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("private key perm = %o, want 600", perm)
	}
	dotPub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatalf("read .pub: %v", err)
	}
	if strings.TrimSpace(string(dotPub)) != pub1 {
		t.Errorf(".pub %q != returned %q", strings.TrimSpace(string(dotPub)), pub1)
	}

	pub2, err := EnsureServerKey(keyPath)
	if err != nil {
		t.Fatalf("second EnsureServerKey: %v", err)
	}
	if pub2 != pub1 {
		t.Errorf("re-read public line changed: %q != %q", pub2, pub1)
	}
}

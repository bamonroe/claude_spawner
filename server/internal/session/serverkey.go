package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// EnsureServerKey makes the server's OWN SSH identity self-managing: the keypair the
// server authenticates WITH for loopback (and any host without a per-host identity),
// kept separate from the host's own `~/.ssh` keys. If keyPath already holds a private
// key it's read and its public line returned; otherwise a fresh ed25519 keypair is
// generated, the private key written 0600 at keyPath (parents created) and the public
// key written to keyPath+".pub" as an authorized_keys line. Either way the public line
// is returned so the caller can log it / point the operator at it.
//
// Installing that public line into the target host's ~/.ssh/authorized_keys is what
// lets the containerized server SSH into the host — for host turns and, crucially, for
// the restart button (SSHes to loopback to rebuild the container). Nothing needs to be
// placed by hand: the server comes up bare and mints this key on first boot.
func EnsureServerKey(keyPath string) (pubLine string, err error) {
	if keyPath == "" {
		return "", fmt.Errorf("server key: empty path")
	}
	if data, rerr := os.ReadFile(keyPath); rerr == nil {
		signer, perr := ssh.ParsePrivateKey(data)
		if perr != nil {
			return "", fmt.Errorf("server key: parse existing %s: %w", keyPath, perr)
		}
		return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " spawner-server", nil
	} else if !os.IsNotExist(rerr) {
		return "", fmt.Errorf("server key: read %s: %w", keyPath, rerr)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", fmt.Errorf("server key: generate: %w", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "spawner-server")
	if err != nil {
		return "", fmt.Errorf("server key: marshal private: %w", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", fmt.Errorf("server key: public: %w", err)
	}
	authLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " spawner-server"

	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return "", fmt.Errorf("server key: mkdir %s: %w", filepath.Dir(keyPath), err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		return "", fmt.Errorf("server key: write %s: %w", keyPath, err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte(authLine+"\n"), 0o644); err != nil {
		return "", fmt.Errorf("server key: write %s.pub: %w", keyPath, err)
	}
	return authLine, nil
}

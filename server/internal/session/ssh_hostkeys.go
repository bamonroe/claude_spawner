package session

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// errKeyScanned aborts the handshake in scanHostKeys the moment the host key is
// captured — we want the key, not a login.
var errKeyScanned = errors.New("host key captured")

// scanHostKeys connects to addr just far enough to capture the host key(s) it
// presents, without authenticating — the SSH equivalent of ssh-keyscan. It probes
// the default algorithm and RSA, so both an ed25519 and an RSA host key are recorded
// when the server offers them. Success is "a key was captured", regardless of the
// (expected) handshake abort.
func scanHostKeys(addr string, timeout time.Duration) ([]ssh.PublicKey, error) {
	var keys []ssh.PublicKey
	seen := map[string]bool{}
	capture := func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if m := string(key.Marshal()); !seen[m] {
			seen[m] = true
			keys = append(keys, key)
		}
		return errKeyScanned
	}
	var lastErr error
	for _, algos := range [][]string{nil, {ssh.KeyAlgoRSA, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSASHA512}} {
		cfg := &ssh.ClientConfig{
			User:              "keyscan",
			HostKeyCallback:   capture,
			HostKeyAlgorithms: algos,
			Timeout:           timeout,
		}
		conn, err := ssh.Dial("tcp", addr, cfg)
		if conn != nil {
			_ = conn.Close()
		}
		if err != nil && !errors.Is(err, errKeyScanned) {
			lastErr = err
		}
	}
	if len(keys) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no host key presented")
		}
		return nil, fmt.Errorf("ssh: scan %s: %w", addr, lastErr)
	}
	return keys, nil
}

// TrustHost scans address:port's host key(s) and records them in known_hosts
// (trust-on-first-use), then reloads verification so the key takes effect without a
// restart. Idempotent: a key already present is skipped, so re-saving a host is a
// no-op. Call sites treat failure as best-effort (an unreachable host just isn't
// recorded yet).
func (p *SSHPool) TrustHost(address string, port int) error {
	if address == "" {
		return fmt.Errorf("ssh: trust needs an address")
	}
	keys, err := scanHostKeys(net.JoinHostPort(address, portStr(port)), p.cfg.timeout())
	if err != nil {
		return err
	}
	entry := knownhosts.Normalize(net.JoinHostPort(address, portStr(port)))
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, _ := os.ReadFile(p.knownHosts)
	var add strings.Builder
	for _, k := range keys {
		line := knownhosts.Line([]string{entry}, k)
		if strings.Contains(string(existing), strings.TrimSpace(line)) {
			continue // already trusted
		}
		add.WriteString(line)
		add.WriteByte('\n')
	}
	if add.Len() > 0 {
		f, err := os.OpenFile(p.knownHosts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.WriteString(add.String()); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return p.reloadHostKeysLocked()
}

// ForgetHost removes every known_hosts entry for address:port and reloads
// verification, so a decommissioned or re-keyed host is no longer trusted.
func (p *SSHPool) ForgetHost(address string, port int) error {
	if address == "" {
		return fmt.Errorf("ssh: forget needs an address")
	}
	entry := knownhosts.Normalize(net.JoinHostPort(address, portStr(port)))
	p.mu.Lock()
	defer p.mu.Unlock()
	data, err := os.ReadFile(p.knownHosts)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var kept []string
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || knownHostsLineMatches(line, entry) {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	if err := os.WriteFile(p.knownHosts, []byte(out), 0o600); err != nil {
		return err
	}
	return p.reloadHostKeysLocked()
}

// reloadHostKeysLocked rebuilds the host-key verifier from the (just-edited)
// known_hosts file. Caller holds p.mu.
func (p *SSHPool) reloadHostKeysLocked() error {
	cb, err := knownhosts.New(p.knownHosts)
	if err != nil {
		return fmt.Errorf("ssh: reload known_hosts %s: %w", p.knownHosts, err)
	}
	p.hostKey = cb
	return nil
}

// knownHostsLineMatches reports whether a plaintext known_hosts line's host field
// names entry. (Entries written here are plaintext, as are ssh-keyscan seeds.)
func knownHostsLineMatches(line, entry string) bool {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	for _, h := range strings.Split(fields[0], ",") {
		if h == entry {
			return true
		}
	}
	return false
}

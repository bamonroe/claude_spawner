package session

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	// sshDefaultPort is the TCP port used when SSHConfig.Port is unset.
	sshDefaultPort = 22
	// sshDialTimeout is the default dial+handshake timeout (SSHConfig.Timeout==0).
	sshDialTimeout = 15 * time.Second
	// sshKeepaliveInterval is how often the pool pings a cached connection so a dead
	// link is detected (and dropped, forcing the next turn to re-dial) promptly.
	sshKeepaliveInterval = 30 * time.Second
)

// SSHConfig describes how to reach remote hosts for SSH-native execution. One
// config serves every host in the pool — a single login user, key, and known-hosts
// file — matching the typical single-account setup; per-host overrides can come
// later. Host keys are always verified (there is deliberately no insecure/skip
// mode): an unknown or changed host key fails the dial.
type SSHConfig struct {
	// User is the SSH login user; empty falls back to the current OS user.
	User string
	// Port is the SSH TCP port; 0 means sshDefaultPort (22).
	Port int
	// KeyFile is a private-key path for public-key auth; empty relies on the agent
	// (SSH_AUTH_SOCK) alone. At least one of the two must yield a usable key.
	KeyFile string
	// KnownHosts is the known_hosts path used to verify host keys; empty falls back
	// to ~/.ssh/known_hosts.
	KnownHosts string
	// Bin is the claude binary on the remote host; empty means "claude".
	Bin string
	// Timeout bounds the dial+handshake; 0 means sshDialTimeout.
	Timeout time.Duration
}

// resolveKnownHostsPath returns the known_hosts path to verify against: the
// configured one, else ~/.ssh/known_hosts. The file is the server's own trust set
// (host keys are added/removed via TrustHost/ForgetHost as the host registry
// changes), shared by every pooled host.
func resolveKnownHostsPath(kh string) (string, error) {
	if kh != "" {
		return kh, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("ssh: locate home for known_hosts: %w", err)
	}
	return filepath.Join(home, ".ssh", "known_hosts"), nil
}

// resolveUser returns the login user: the given per-host user, else the config
// default, else the server's current OS user.
func (c SSHConfig) resolveUser(host string) (string, error) {
	if host != "" {
		return host, nil
	}
	if c.User != "" {
		return c.User, nil
	}
	u, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("ssh: resolve current user: %w", err)
	}
	return u.Username, nil
}

// authMethods gathers auth: the ssh-agent (if present), an explicit key file (the
// per-host/identity key, else the config default), and a password (from a managed
// identity) as a fallback. At least one usable method is required.
func (c SSHConfig) authMethods(keyFile, password string) ([]ssh.AuthMethod, error) {
	if keyFile == "" && password == "" {
		keyFile = c.KeyFile
	}
	var auths []ssh.AuthMethod
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			auths = append(auths, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if keyFile != "" {
		key, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("ssh: read key %s: %w", keyFile, err)
		}
		signer, err := ssh.ParsePrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("ssh: parse key %s: %w", keyFile, err)
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if password != "" {
		auths = append(auths, ssh.Password(password))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh: no auth method (set a key file, a password, or run an ssh-agent)")
	}
	return auths, nil
}

func (c SSHConfig) timeout() time.Duration {
	if c.Timeout == 0 {
		return sshDialTimeout
	}
	return c.Timeout
}

// portStr renders an SSH port, defaulting 0 to 22.
func portStr(p int) string {
	if p == 0 {
		p = sshDefaultPort
	}
	return strconv.Itoa(p)
}

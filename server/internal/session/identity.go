package session

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
)

// Identity is a managed SSH keypair the app creates and hosts reference by name.
// The PRIVATE key never leaves the server — it's what the server authenticates with
// — so the wire form carries only the PublicKey (an authorized_keys line) for the
// user to copy onto a target host. The private key lives at keysDir/<sanitized name>
// with 0600 permissions; KeyPath derives it from the name (nothing else stores it).
type Identity struct {
	Name string `json:"name"`
	// User is the default SSH login user for this identity; a host's own User
	// overrides it. Required.
	User string `json:"user"`
	// PublicKey is the authorized_keys line — safe to display/copy. Empty for a
	// password-only identity (no keypair).
	PublicKey string `json:"public_key,omitempty"`
	// Password authenticates via SSH password auth. It is SERVER-ONLY — persisted
	// here but never sent to the app (the wire form reports only whether one is set).
	Password string `json:"password,omitempty"`
}

// IdentityStore persists the identity registry (names + public keys) as JSON and
// keeps each private key as a 0600 file under keysDir. It mirrors HostStore's atomic
// temp+rename persistence and is safe for concurrent use. The app is the source of
// truth for which identities exist; the server owns the private key material.
type IdentityStore struct {
	path    string
	keysDir string
	mu      sync.RWMutex
	byName  map[string]*Identity
}

// OpenIdentityStore loads (or initializes) the registry at path, with private keys
// kept under keysDir. A missing registry is a fresh, empty store.
func OpenIdentityStore(path, keysDir string) (*IdentityStore, error) {
	s := &IdentityStore{path: path, keysDir: keysDir, byName: map[string]*Identity{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	var list []*Identity
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse identities %s: %w", path, err)
	}
	for _, id := range list {
		if id.Name != "" {
			s.byName[id.Name] = id
		}
	}
	return s, nil
}

// List returns all identities sorted by name.
func (s *IdentityStore) List() []*Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Identity, 0, len(s.byName))
	for _, id := range s.byName {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the identity by name, or nil.
func (s *IdentityStore) Get(name string) *Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.byName[name]
}

// KeyPath is where the private key for an identity lives on the server. It's derived
// purely from the name, so the SSH pool can resolve a host's identity to a key file
// without consulting the registry.
func (s *IdentityStore) KeyPath(name string) string {
	return filepath.Join(s.keysDir, sanitizeIdentityName(name))
}

// Create registers a new identity for the required user. When genKey is set it
// generates a fresh ed25519 keypair (private key written 0600 under keysDir, public
// key recorded); password, if given, adds SSH password auth. A keyless identity must
// carry a password (otherwise it has no way to authenticate). Errors if the name is
// empty/taken or the user is empty — regenerating a taken name would invalidate any
// host already trusting the old key.
func (s *IdentityStore) Create(name, user, password string, genKey bool) (*Identity, error) {
	name, user = strings.TrimSpace(name), strings.TrimSpace(user)
	if name == "" {
		return nil, fmt.Errorf("identity needs a name")
	}
	if user == "" {
		return nil, fmt.Errorf("identity needs a username")
	}
	if !genKey && password == "" {
		return nil, fmt.Errorf("identity needs a key or a password")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[name]; ok {
		return nil, fmt.Errorf("identity %q already exists", name)
	}

	var priv []byte
	var authLine string
	if genKey {
		pub, pk, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("generate key: %w", err)
		}
		pemBlock, err := ssh.MarshalPrivateKey(pk, name)
		if err != nil {
			return nil, fmt.Errorf("marshal private key: %w", err)
		}
		sshPub, err := ssh.NewPublicKey(pub)
		if err != nil {
			return nil, fmt.Errorf("public key: %w", err)
		}
		// authorized_keys line with the identity name as the comment.
		authLine = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " " + name
		priv = pem.EncodeToMemory(pemBlock)
	}
	return s.record(name, user, priv, authLine, password)
}

// Import registers an existing private key (already on the server, e.g. the config
// default SSH key) as a managed identity for the required user: it copies the key
// into keysDir under name and records its public key. The original file is left
// untouched; encrypted keys are rejected (the server authenticates non-interactively).
func (s *IdentityStore) Import(name, user, password, srcPath string) (*Identity, error) {
	name, user = strings.TrimSpace(name), strings.TrimSpace(user)
	if name == "" {
		return nil, fmt.Errorf("identity needs a name")
	}
	if user == "" {
		return nil, fmt.Errorf("identity needs a username")
	}
	if strings.TrimSpace(srcPath) == "" {
		return nil, fmt.Errorf("need a private-key path to import")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[name]; ok {
		return nil, fmt.Errorf("identity %q already exists", name)
	}
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		return nil, fmt.Errorf("read key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("parse key (encrypted keys are not supported): %w", err)
	}
	authLine := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " " + name
	return s.record(name, user, raw, authLine, password)
}

// record writes the private key (0600, only when there is one) and registers +
// persists the identity. The caller holds s.mu.
func (s *IdentityStore) record(name, user string, priv []byte, authLine, password string) (*Identity, error) {
	if priv != nil {
		if err := os.MkdirAll(s.keysDir, 0o700); err != nil {
			return nil, fmt.Errorf("keys dir: %w", err)
		}
		if err := os.WriteFile(s.KeyPath(name), priv, 0o600); err != nil {
			return nil, fmt.Errorf("write private key: %w", err)
		}
	}
	id := &Identity{Name: name, User: user, PublicKey: authLine, Password: password}
	s.byName[name] = id
	if err := s.flush(); err != nil {
		return nil, err
	}
	return id, nil
}

// Update changes an existing identity's login user and, when setPassword is true,
// its SSH password (an empty password then clears it). The keypair is left as-is —
// regenerating it would invalidate any host already trusting the public key, so it's
// never touched here. Errors if the identity is unknown, the user is empty, or the
// change would leave a key-less identity with no password (hence no way to auth).
func (s *IdentityStore) Update(name, user string, setPassword bool, password string) (*Identity, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return nil, fmt.Errorf("identity needs a username")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("no identity %q", name)
	}
	newPassword := id.Password
	if setPassword {
		newPassword = password
	}
	if id.PublicKey == "" && newPassword == "" {
		return nil, fmt.Errorf("a key-less identity needs a password")
	}
	id.User = user
	id.Password = newPassword
	if err := s.flush(); err != nil {
		return nil, err
	}
	return id, nil
}

// Delete removes an identity and its private key file. Missing key files are
// ignored so a partially-created identity can still be cleaned up.
func (s *IdentityStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byName[name]; !ok {
		return fmt.Errorf("no identity %q", name)
	}
	if err := os.Remove(s.KeyPath(name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove private key: %w", err)
	}
	delete(s.byName, name)
	return s.flush()
}

// flush atomically writes the registry (temp file + rename). Callers hold s.mu.
func (s *IdentityStore) flush() error {
	list := make([]*Identity, 0, len(s.byName))
	for _, id := range s.byName {
		list = append(list, id)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// sanitizeIdentityName keeps an identity name safe to use as a filename.
func sanitizeIdentityName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "identity"
	}
	return "id_" + out
}

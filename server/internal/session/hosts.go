package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Host is a configured SSH target for SSH-native execution. The app is the source
// of truth for the host list (all editing happens in-app); the server persists it
// to a JSON file so it survives restarts and is shared across clients. A session's
// Host field names one of these entries; the SSH pool resolves the name to these
// connection details when dialing.
type Host struct {
	// Name is the logical handle (what Session.Host stores and the spawn dialog
	// offers), e.g. "work" or "local".
	Name string `json:"name"`
	// Address is the hostname/IP the SSH pool dials. NOTE: the Go SSH client dials
	// this literally and does NOT read ~/.ssh/config, so this must be a real
	// hostname/IP (e.g. a Tailscale IP), not a ~/.ssh/config alias.
	Address string `json:"address"`
	// User is the SSH login user; empty falls back to the server's OS user.
	User string `json:"user,omitempty"`
	// Port is the SSH port; 0 means 22.
	Port int `json:"port,omitempty"`
	// KeyFile is a server-side private-key path for this host; empty relies on the
	// ssh-agent. (The app configures the path; key material stays on the server.)
	// Superseded by Identity when that is set.
	KeyFile string `json:"key_file,omitempty"`
	// Identity names a managed IdentityStore keypair to authenticate with. When set,
	// the pool uses that identity's server-side private key and KeyFile is ignored —
	// this is the app-managed alternative to a raw KeyFile path.
	Identity string `json:"identity,omitempty"`
	// ClaudeBin is the claude binary on this host; empty means "claude".
	ClaudeBin string `json:"claude_bin,omitempty"`
}

// HostStore is a concurrency-safe, file-backed registry of configured Hosts,
// mutated only via the app's hosts settings (list/put/delete over the wire) and
// persisted atomically so it survives restarts.
type HostStore struct {
	path   string
	mu     sync.RWMutex
	byName map[string]*Host
}

// OpenHostStore loads (or initializes) the host registry at path.
func OpenHostStore(path string) (*HostStore, error) {
	h := &HostStore{path: path, byName: map[string]*Host{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Fresh registry: seed the loopback host so a new deployment lists it out
			// of the box. It's an ordinary entry — editable and deletable like any
			// other. Once the user touches the registry the file exists (any Put/Delete
			// flushes it), so this never re-seeds and a delete sticks. A deployment
			// whose box can't reach itself over SSH just removes it and drives remotes.
			h.byName[LocalHost] = &Host{Name: LocalHost, Address: LocalHost}
			return h, nil
		}
		return nil, err
	}
	var list []*Host
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("parse hosts %s: %w", path, err)
	}
	for _, host := range list {
		if host.Name != "" {
			h.byName[host.Name] = host
		}
	}
	return h, nil
}

// List returns all configured hosts, sorted by name.
func (h *HostStore) List() []*Host {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*Host, 0, len(h.byName))
	for _, host := range h.byName {
		out = append(out, host)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get returns the configured host by name, or nil.
func (h *HostStore) Get(name string) *Host {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byName[name]
}

// Put upserts a host by name and persists the registry.
func (h *HostStore) Put(host *Host) error {
	if host == nil || host.Name == "" {
		return fmt.Errorf("host needs a name")
	}
	h.mu.Lock()
	h.byName[host.Name] = host
	h.mu.Unlock()
	return h.flush()
}

// Delete removes a host by name (no error if absent) and persists.
func (h *HostStore) Delete(name string) error {
	h.mu.Lock()
	delete(h.byName, name)
	h.mu.Unlock()
	return h.flush()
}

// flush writes the registry atomically (temp file + rename).
func (h *HostStore) flush() error {
	h.mu.RLock()
	list := make([]*Host, 0, len(h.byName))
	for _, host := range h.byName {
		list = append(list, host)
	}
	h.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })

	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(h.path), 0o755); err != nil {
		return err
	}
	tmp := h.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, h.path)
}

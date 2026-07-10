package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Settings holds server-global, runtime-changeable preferences that must outlive a
// restart. Env vars (internal/config) set the boot defaults; anything the user
// changes live over the wire (e.g. the resident whisper model) is captured here so
// a reboot or rebuild doesn't silently revert it. Fields are omitempty so an unset
// preference falls back to the env default rather than pinning a zero value.
type Settings struct {
	// WhisperModel is the resident whisper server's model NAME (e.g. "medium.en"),
	// last selected from a client. Empty means "use SPAWNER_WHISPER_MODEL_NAME".
	WhisperModel string `json:"whisper_model,omitempty"`
}

// SettingsStore is a concurrency-safe, file-backed holder for the server's
// persisted Settings. It mirrors the Store pattern used for sessions/hosts: load on
// boot, flush atomically on every change. An empty path yields an in-memory-only
// store (flush is a no-op) so the server still runs when SPAWNER_STATE is unset.
type SettingsStore struct {
	path string
	mu   sync.RWMutex
	s    Settings
}

// OpenSettings loads (or initializes) the settings file at path.
func OpenSettings(path string) (*SettingsStore, error) {
	st := &SettingsStore{path: path}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil // fresh store
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &st.s); err != nil {
		return nil, fmt.Errorf("parse settings %s: %w", path, err)
	}
	return st, nil
}

// Get returns a copy of the current settings.
func (st *SettingsStore) Get() Settings {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s
}

// WhisperModel returns the persisted resident-whisper model name (empty if none).
func (st *SettingsStore) WhisperModel() string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.s.WhisperModel
}

// SetWhisperModel records the resident-whisper model name and persists the file.
func (st *SettingsStore) SetWhisperModel(name string) error {
	st.mu.Lock()
	if st.s.WhisperModel == name {
		st.mu.Unlock()
		return nil
	}
	st.s.WhisperModel = name
	st.mu.Unlock()
	return st.flush()
}

// flush writes the settings atomically (temp file + rename). No-op with no path.
func (st *SettingsStore) flush() error {
	if st.path == "" {
		return nil
	}
	st.mu.RLock()
	data, err := json.MarshalIndent(st.s, "", "  ")
	st.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

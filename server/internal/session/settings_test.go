package session

import (
	"path/filepath"
	"testing"
)

func TestSettingsStorePersistsWhisperModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	st, err := OpenSettings(path)
	if err != nil {
		t.Fatalf("OpenSettings: %v", err)
	}
	if got := st.WhisperModel(); got != "" {
		t.Fatalf("fresh store: got %q, want empty", got)
	}
	if err := st.SetWhisperModel("large-v3"); err != nil {
		t.Fatalf("SetWhisperModel: %v", err)
	}

	// Reopen: the choice must survive (i.e. it was written to disk).
	st2, err := OpenSettings(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := st2.WhisperModel(); got != "large-v3" {
		t.Fatalf("after reopen: got %q, want large-v3", got)
	}
}

func TestSettingsStoreEmptyPathIsInMemory(t *testing.T) {
	st, err := OpenSettings("")
	if err != nil {
		t.Fatalf("OpenSettings(\"\"): %v", err)
	}
	// No path → flush is a no-op, but the value is still readable in-memory.
	if err := st.SetWhisperModel("medium.en"); err != nil {
		t.Fatalf("SetWhisperModel: %v", err)
	}
	if got := st.WhisperModel(); got != "medium.en" {
		t.Fatalf("in-memory: got %q, want medium.en", got)
	}
}

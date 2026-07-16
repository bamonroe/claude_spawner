package session

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestSettingKVPersistsAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings_kv.json")

	s, err := OpenSettingKV(path)
	if err != nil {
		t.Fatalf("OpenSettingKV: %v", err)
	}
	if got := s.Value("whisper_model"); got != "" {
		t.Fatalf("fresh store: got %q, want empty", got)
	}
	if err := s.Put(&SettingRecord{Key: "whisper_model", Value: "large-v3", UpdatedAt: 100}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Put(&SettingRecord{Key: "summary_only", Value: "true", UpdatedAt: 100}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	s2, err := OpenSettingKV(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := s2.Value("whisper_model"); got != "large-v3" {
		t.Fatalf("after reopen: got %q, want large-v3", got)
	}
	if got := s2.Value("summary_only"); got != "true" {
		t.Fatalf("after reopen: got %q, want true", got)
	}
}

func TestSettingKVLastWriterWins(t *testing.T) {
	s, _ := OpenSettingKV("")
	if err := s.Put(&SettingRecord{Key: "auto_compress", Value: "true", UpdatedAt: 200}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// An older write is rejected (stale).
	if err := s.Put(&SettingRecord{Key: "auto_compress", Value: "false", UpdatedAt: 100}); !errors.Is(err, ErrStale) {
		t.Fatalf("older write: got %v, want ErrStale", err)
	}
	if got := s.Value("auto_compress"); got != "true" {
		t.Fatalf("value after stale write: got %q, want true", got)
	}
	// A newer write wins.
	if err := s.Put(&SettingRecord{Key: "auto_compress", Value: "false", UpdatedAt: 300}); err != nil {
		t.Fatalf("newer Put: %v", err)
	}
	if got := s.Value("auto_compress"); got != "false" {
		t.Fatalf("value after newer write: got %q, want false", got)
	}
}

func TestSettingKVDeleteTombstones(t *testing.T) {
	s, _ := OpenSettingKV("")
	_ = s.Put(&SettingRecord{Key: "summary_only", Value: "true", UpdatedAt: 200})
	if err := s.Delete("summary_only", 300); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := s.Value("summary_only"); got != "" {
		t.Fatalf("after delete: got %q, want empty", got)
	}
	// A stale re-add (older than the tombstone) is blocked.
	if err := s.Put(&SettingRecord{Key: "summary_only", Value: "true", UpdatedAt: 250}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale re-add: got %v, want ErrStale", err)
	}
	// A newer re-add resurrects it.
	if err := s.Put(&SettingRecord{Key: "summary_only", Value: "false", UpdatedAt: 400}); err != nil {
		t.Fatalf("newer re-add: %v", err)
	}
	if got := s.Value("summary_only"); got != "false" {
		t.Fatalf("after resurrect: got %q, want false", got)
	}
}

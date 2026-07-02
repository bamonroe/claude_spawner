// Package config holds server configuration, loaded from environment variables.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the server's runtime configuration.
type Config struct {
	// Addr is the listen address, e.g. ":8080".
	Addr string
	// AuthToken is the shared secret the app must present in its hello message.
	// The server refuses to start if this is empty (no unauthenticated mode).
	AuthToken string
	// SpawnRoots constrains where sessions may be created and searched. A spawn
	// path must live under one of these. Empty means "no restriction" (NOT
	// recommended). Parsed from SPAWNER_ROOT as a ":"-separated list (like PATH).
	SpawnRoots []string
	// StatePath is the file where the durable session registry is persisted.
	StatePath string
	// ClaudeBin is the claude binary used for headless turns and babysit panes.
	ClaudeBin string
	// WhisperBin is the whisper.cpp CLI (default "whisper-cli").
	WhisperBin string
	// WhisperURL points at a resident whisper.cpp server (its base URL, e.g.
	// http://localhost:8571). When set, transcription goes there instead of
	// forking whisper-cli locally.
	WhisperURL string
	// WhisperModel is the path to a ggml model file. Empty disables transcription
	// (the audio path returns not_implemented; text utterances still work).
	WhisperModel string
	// WhisperModelFast is an optional smaller/faster model used for short clips
	// (quick confirmations like "yes"). Empty = always use WhisperModel.
	WhisperModelFast string
	// WhisperModelBase is an optional middle model, selectable in "fixed" mode.
	WhisperModelBase string
	// WhisperFastMaxSeconds is the clip-length cutoff for using the fast model.
	WhisperFastMaxSeconds float64
	// WhisperLang biases decoding (e.g. "en"); empty = auto-detect.
	WhisperLang string
	// FfmpegBin decodes compressed audio (Ogg/Opus) to WAV for whisper.
	FfmpegBin string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:             env("SPAWNER_ADDR", ":8080"),
		AuthToken:        os.Getenv("SPAWNER_TOKEN"),
		StatePath:        env("SPAWNER_STATE", "sessions.json"),
		ClaudeBin:        env("SPAWNER_CLAUDE_BIN", "claude"),
		WhisperBin:       env("SPAWNER_WHISPER_BIN", "whisper-cli"),
		WhisperURL:       os.Getenv("SPAWNER_WHISPER_URL"),
		WhisperModel:     os.Getenv("SPAWNER_WHISPER_MODEL"),
		WhisperModelFast: os.Getenv("SPAWNER_WHISPER_MODEL_FAST"),
		WhisperModelBase: os.Getenv("SPAWNER_WHISPER_MODEL_BASE"),
		WhisperLang:      env("SPAWNER_WHISPER_LANG", "en"),
		FfmpegBin:        env("SPAWNER_FFMPEG_BIN", "ffmpeg"),
	}
	if c.AuthToken == "" {
		return nil, fmt.Errorf("SPAWNER_TOKEN is required (refusing to run without auth)")
	}
	c.WhisperFastMaxSeconds = 2.5 // default cutoff
	if v := os.Getenv("SPAWNER_WHISPER_FAST_MAX_SEC"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			c.WhisperFastMaxSeconds = f
		}
	}
	for _, r := range strings.Split(os.Getenv("SPAWNER_ROOT"), ":") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_ROOT %q: %w", r, err)
		}
		c.SpawnRoots = append(c.SpawnRoots, abs)
	}
	return c, nil
}

// ValidateSpawnDir checks that `dir` lives under one of the configured roots.
// Returns the cleaned absolute path or an error if it escapes all roots.
func (c *Config) ValidateSpawnDir(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if len(c.SpawnRoots) == 0 {
		return abs, nil
	}
	for _, root := range c.SpawnRoots {
		if under(root, abs) {
			return abs, nil
		}
	}
	return "", fmt.Errorf("path %q is outside the allowed roots %v", abs, c.SpawnRoots)
}

// under reports whether abs is root itself or nested within it.
func under(root, abs string) bool {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

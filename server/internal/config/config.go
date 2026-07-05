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
	// ClaudeBin is the claude binary used for headless turns.
	ClaudeBin string
	// WhisperBin is the whisper.cpp CLI (default "whisper-cli").
	WhisperBin string
	// WhisperURL points at a resident whisper.cpp server (its base URL, e.g.
	// http://localhost:8571). When set, transcription goes there instead of
	// forking whisper-cli locally.
	WhisperURL string
	// WhisperFastURL points at a second, fast whisper server (e.g. base.en) used
	// only for the live hands-free draft + end-token detection, so those don't
	// queue behind the accurate model. Empty → drafts use the main server.
	WhisperFastURL string
	// WhisperModelName is the resident whisper server's default model NAME
	// (small.en | medium.en | large-v3), loaded at startup and reported to apps.
	// The model is server-global: apps read it on connect and change it via an
	// explicit push, so two clients don't bounce it around.
	WhisperModelName string
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
	// SandboxImage is the container image used for sessions whose target is
	// "sandbox". Empty disables the sandbox target (spawns can then only run on the
	// host). See the containerized-server design in docs/architecture.md.
	SandboxImage string
	// SandboxRuntime is the container CLI for sandbox sessions (default "podman"
	// for rootless — launching a sandbox then needs no host root).
	SandboxRuntime string
	// SandboxClaudeBin is the claude binary inside the sandbox image (default
	// "claude").
	SandboxClaudeBin string
	// SandboxMounts are extra `-v` volume specs ("host:container[:opts]") for
	// sandbox sessions, comma-separated. Typically shares "$HOME/.claude" so
	// in-sandbox transcripts stay discoverable by the host.
	SandboxMounts []string
	// SandboxRunArgs are extra container `run` flags for sandbox sessions,
	// space-separated (e.g. "--userns=keep-id --network=none").
	SandboxRunArgs []string
	// BrokerSocket, when set, is the Unix socket of a host-side broker daemon. The
	// server then runs "host"-target turns through the broker instead of forking
	// claude itself — the arrangement that lets a containerized, unprivileged
	// server run turns on the host. Empty = fork claude directly (native install).
	BrokerSocket string
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
		WhisperFastURL:   os.Getenv("SPAWNER_WHISPER_FAST_URL"),
		WhisperModelName: env("SPAWNER_WHISPER_MODEL_NAME", "medium.en"),
		WhisperModel:     os.Getenv("SPAWNER_WHISPER_MODEL"),
		WhisperModelFast: os.Getenv("SPAWNER_WHISPER_MODEL_FAST"),
		WhisperModelBase: os.Getenv("SPAWNER_WHISPER_MODEL_BASE"),
		WhisperLang:      env("SPAWNER_WHISPER_LANG", "en"),
		FfmpegBin:        env("SPAWNER_FFMPEG_BIN", "ffmpeg"),
		SandboxImage:     os.Getenv("SPAWNER_SANDBOX_IMAGE"),
		SandboxRuntime:   env("SPAWNER_SANDBOX_RUNTIME", "podman"),
		SandboxClaudeBin: env("SPAWNER_SANDBOX_CLAUDE_BIN", "claude"),
		SandboxMounts:    splitList(os.Getenv("SPAWNER_SANDBOX_MOUNTS"), ","),
		SandboxRunArgs:   strings.Fields(os.Getenv("SPAWNER_SANDBOX_RUN_ARGS")),
		BrokerSocket:     os.Getenv("SPAWNER_BROKER_SOCKET"),
	}
	if c.AuthToken == "" {
		return nil, fmt.Errorf("SPAWNER_TOKEN is required (refusing to run without auth)")
	}
	c.WhisperFastMaxSeconds = 2.5 // default cutoff
	if v := os.Getenv("SPAWNER_WHISPER_FAST_MAX_SEC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_WHISPER_FAST_MAX_SEC %q: %w", v, err)
		}
		c.WhisperFastMaxSeconds = f
	}
	roots, err := ParseRoots(os.Getenv("SPAWNER_ROOT"))
	if err != nil {
		return nil, err
	}
	c.SpawnRoots = roots
	return c, nil
}

// ParseRoots parses a SPAWNER_ROOT value (":"-separated, like PATH) into absolute
// directories, skipping empties. Exported so the standalone broker can build the
// same spawn jail without loading the full server config (it has no auth token).
func ParseRoots(s string) ([]string, error) {
	var roots []string
	for _, r := range strings.Split(s, ":") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_ROOT %q: %w", r, err)
		}
		roots = append(roots, abs)
	}
	return roots, nil
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

// under reports whether abs is root itself or nested within it. This is the
// path-traversal jail check for spawn dirs, so each clause matters:
//   - rel == "."                     → abs IS root (allowed)
//   - rel == ".." or "../…"          → abs escapes upward out of root (rejected)
//   - filepath.IsAbs(rel)            → different volume/root entirely (rejected;
//     Rel returns an absolute path when the two share no common base)
//
// Everything else is a genuine descendant of root.
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

// splitList splits s on sep, trimming whitespace and dropping empty fields.
func splitList(s, sep string) []string {
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

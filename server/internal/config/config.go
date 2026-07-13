// Package config holds server configuration, loaded from environment variables.
package config

import (
	"crypto/tls"
	"crypto/x509"
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
	// HostsPath is the file where the app-managed SSH host registry is persisted
	// (the source of truth is the app; the server just stores it so it survives
	// restarts and is shared across clients).
	HostsPath string
	// IdentitiesPath is the file where the app-managed SSH identity registry (names
	// + public keys) is persisted; SSHKeysDir is where each identity's private key
	// is kept (0600). The app creates identities and copies their public keys; the
	// private material never leaves the server.
	IdentitiesPath string
	SSHKeysDir     string
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
	// WhisperFastModelName is the fast (draft/detection) server's default model
	// NAME, same semantics and lifecycle as WhisperModelName. Only meaningful
	// when WhisperFastURL is set.
	WhisperFastModelName string
	// WhisperModelsDir is the host directory holding the ggml model files the
	// resident whisper servers mount at /models. When set, the gateway lists
	// its ggml-*.bin files so clients can offer a model picker; empty = no list.
	WhisperModelsDir string
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
	// SandboxCodexBin is the codex binary inside the sandbox image (default
	// "codex"), for Codex-backend sandbox sessions.
	SandboxCodexBin string
	// SandboxMounts are extra `-v` volume specs ("host:container[:opts]") for
	// sandbox sessions, comma-separated. Typically shares "$HOME/.claude" so
	// in-sandbox transcripts stay discoverable by the host.
	SandboxMounts []string
	// SandboxRunArgs are extra container `run` flags for sandbox sessions,
	// space-separated (e.g. "--userns=keep-id --network=none").
	SandboxRunArgs []string
	// RestartCmd is a shell command (run via `sh -c`, detached) that rebuilds and
	// relaunches the server for the app's "restart" button — it SSHes to the host
	// and launches deploy/rebuild-container.sh detached (setsid), which runs
	// `compose up -d --build` to rebuild the image and recreate this container. It
	// is fired fire-and-forget in its own process group so it survives the server's
	// own teardown as the container is recreated.
	// Empty disables restart (the app's button reports it isn't configured).
	RestartCmd string
	// TLSCert and TLSKey are the PEM cert/key files for serving wss:// (HTTPS).
	// Both or neither: setting one without the other is a config error. When both
	// are set the listener serves TLS; empty means plain ws:// (fine behind a
	// Tailscale/WireGuard tunnel, which already encrypts the hop).
	TLSCert string
	TLSKey  string
	// TLSClientCA is a PEM bundle of certificate authorities that sign the client
	// certificates the app must present — enabling mutual TLS. When set, a client
	// must prove possession of a private key signed by one of these CAs *in
	// addition to* the shared token, so a leaked token alone can't attach. Requires
	// TLSCert/TLSKey (mTLS is layered on server TLS). Empty = no client-cert check.
	TLSClientCA string
	// SSHUser/SSHPort/SSHKey/SSHKnownHosts/SSHClaudeBin configure the one SSH
	// connection template shared by every host in the pool. SSHUser empty = current
	// OS user; SSHPort 0 = 22; SSHKey empty = rely on the ssh-agent; SSHKnownHosts
	// empty = ~/.ssh/known_hosts (host keys are always verified — no insecure mode);
	// SSHClaudeBin/SSHCodexBin are the remote claude/codex binaries (default
	// "claude"/"codex").
	SSHUser       string
	SSHPort       int
	SSHKey        string
	SSHKnownHosts string
	SSHClaudeBin  string
	SSHCodexBin   string

	// WebDir is a directory holding the built Compose/Wasm web-client bundle
	// (index.html + spawnerweb.js + .wasm). When set, the server serves it as
	// static files at "/" alongside the "/ws" gateway, so one binary hosts both
	// the API and the browser client. Empty disables static serving.
	WebDir string
}

// Load reads configuration from the environment and validates it.
func Load() (*Config, error) {
	c := &Config{
		Addr:                 env("SPAWNER_ADDR", ":8080"),
		AuthToken:            os.Getenv("SPAWNER_TOKEN"),
		WebDir:               os.Getenv("SPAWNER_WEB_DIR"),
		StatePath:            env("SPAWNER_STATE", "sessions.json"),
		HostsPath:            env("SPAWNER_HOSTS", "hosts.json"),
		IdentitiesPath:       env("SPAWNER_IDENTITIES", "identities.json"),
		SSHKeysDir:           env("SPAWNER_SSH_KEYS", "ssh_keys"),
		ClaudeBin:            env("SPAWNER_CLAUDE_BIN", "claude"),
		WhisperBin:           env("SPAWNER_WHISPER_BIN", "whisper-cli"),
		WhisperURL:           os.Getenv("SPAWNER_WHISPER_URL"),
		WhisperFastURL:       os.Getenv("SPAWNER_WHISPER_FAST_URL"),
		WhisperModelName:     env("SPAWNER_WHISPER_MODEL_NAME", "medium.en"),
		WhisperFastModelName: env("SPAWNER_WHISPER_FAST_MODEL_NAME", "base.en"),
		WhisperModelsDir:     os.Getenv("SPAWNER_WHISPER_MODELS_DIR"),
		WhisperModel:         os.Getenv("SPAWNER_WHISPER_MODEL"),
		WhisperModelFast:     os.Getenv("SPAWNER_WHISPER_MODEL_FAST"),
		WhisperModelBase:     os.Getenv("SPAWNER_WHISPER_MODEL_BASE"),
		WhisperLang:          env("SPAWNER_WHISPER_LANG", "en"),
		FfmpegBin:            env("SPAWNER_FFMPEG_BIN", "ffmpeg"),
		SandboxImage:         os.Getenv("SPAWNER_SANDBOX_IMAGE"),
		SandboxRuntime:       env("SPAWNER_SANDBOX_RUNTIME", "podman"),
		SandboxClaudeBin:     env("SPAWNER_SANDBOX_CLAUDE_BIN", "claude"),
		SandboxCodexBin:      env("SPAWNER_SANDBOX_CODEX_BIN", "codex"),
		SandboxMounts:        splitList(os.Getenv("SPAWNER_SANDBOX_MOUNTS"), ","),
		SandboxRunArgs:       strings.Fields(os.Getenv("SPAWNER_SANDBOX_RUN_ARGS")),
		RestartCmd:           os.Getenv("SPAWNER_RESTART_CMD"),
		TLSCert:              os.Getenv("SPAWNER_TLS_CERT"),
		TLSKey:               os.Getenv("SPAWNER_TLS_KEY"),
		TLSClientCA:          os.Getenv("SPAWNER_TLS_CLIENT_CA"),
		SSHUser:              os.Getenv("SPAWNER_SSH_USER"),
		SSHKey:               os.Getenv("SPAWNER_SSH_KEY"),
		SSHKnownHosts:        os.Getenv("SPAWNER_SSH_KNOWN_HOSTS"),
		SSHClaudeBin:         env("SPAWNER_SSH_CLAUDE_BIN", "claude"),
		SSHCodexBin:          env("SPAWNER_SSH_CODEX_BIN", "codex"),
	}
	if c.AuthToken == "" {
		return nil, fmt.Errorf("SPAWNER_TOKEN is required (refusing to run without auth)")
	}
	// The env template ships this literal; anyone who can reach the socket can run
	// arbitrary commands as the server's user, so a well-known token is no auth at all.
	if c.AuthToken == "change-me" {
		return nil, fmt.Errorf("SPAWNER_TOKEN is still the template placeholder %q — set a real secret", c.AuthToken)
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("SPAWNER_TLS_CERT and SPAWNER_TLS_KEY must be set together")
	}
	if c.TLSClientCA != "" && !c.TLSEnabled() {
		return nil, fmt.Errorf("SPAWNER_TLS_CLIENT_CA requires SPAWNER_TLS_CERT and SPAWNER_TLS_KEY (mTLS is layered on server TLS)")
	}
	c.WhisperFastMaxSeconds = 2.5 // default cutoff
	if v := os.Getenv("SPAWNER_WHISPER_FAST_MAX_SEC"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_WHISPER_FAST_MAX_SEC %q: %w", v, err)
		}
		c.WhisperFastMaxSeconds = f
	}
	if v := os.Getenv("SPAWNER_SSH_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_SSH_PORT %q: %w", v, err)
		}
		c.SSHPort = n
	}
	roots, err := ParseRoots(os.Getenv("SPAWNER_ROOT"))
	if err != nil {
		return nil, err
	}
	c.SpawnRoots = roots
	return c, nil
}

// TLSEnabled reports whether the server should serve wss:// — true when both a
// cert and key are configured (Load guarantees they're set together).
func (c *Config) TLSEnabled() bool { return c.TLSCert != "" && c.TLSKey != "" }

// MutualTLS reports whether clients must present a valid client certificate.
func (c *Config) MutualTLS() bool { return c.TLSClientCA != "" }

// BuildTLSConfig constructs the server's *tls.Config, loading the client-CA pool
// and requiring client certs when mTLS is enabled. Returns (nil, nil) when TLS is
// disabled, so callers fall back to a plain listener.
func (c *Config) BuildTLSConfig() (*tls.Config, error) {
	if !c.TLSEnabled() {
		return nil, nil
	}
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	if c.MutualTLS() {
		pem, err := os.ReadFile(c.TLSClientCA)
		if err != nil {
			return nil, fmt.Errorf("SPAWNER_TLS_CLIENT_CA %q: %w", c.TLSClientCA, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("SPAWNER_TLS_CLIENT_CA %q: no PEM certificates found", c.TLSClientCA)
		}
		t.ClientCAs = pool
		t.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return t, nil
}

// ParseRoots parses a SPAWNER_ROOT value (":"-separated, like PATH) into absolute
// directories, skipping empties. Exported so callers can build the same spawn jail
// without loading the full server config.
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

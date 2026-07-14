// Command spawner is the claude_spawner server: it manages durable Claude Code
// sessions (a session_id on disk per directory) and bridges voice/text between
// the Android app and those sessions.
//
// The voice data path drives Claude Code headless in stream-json mode (see
// internal/session). tmux (internal/tmux) is used only to detect a Claude
// session a human already has open interactively in a pane (to warn before
// driving the same session headlessly). See README.md for the roadmap.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bam/claude_spawner/server/internal/agent"
	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/gateway"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
	"github.com/bam/claude_spawner/server/internal/tts"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if len(cfg.SpawnRoots) == 0 {
		log.Printf("WARNING: SPAWNER_ROOT is empty — sessions may be spawned in ANY directory " +
			"(no path jail). Set SPAWNER_ROOT to a colon-separated allow-list to constrain spawn scope.")
	}

	store, err := session.OpenStore(cfg.StatePath)
	if err != nil {
		log.Fatalf("session store: %v", err)
	}
	hostStore, err := session.OpenHostStore(cfg.HostsPath)
	if err != nil {
		log.Fatalf("host store: %v", err)
	}
	idStore, err := session.OpenIdentityStore(cfg.IdentitiesPath, cfg.SSHKeysDir)
	if err != nil {
		log.Fatalf("identity store: %v", err)
	}
	driver := session.NewDriver()
	driver.RestartCmd = cfg.RestartCmd
	// First-run starter profiles, seeded from the flat sandbox config. Written once
	// to SPAWNER_PROFILES; after that the app owns the catalogue. bare-metal (host)
	// is the default so a fresh install works even without a sandbox image; the two
	// sandbox starters are seeded only when an image is configured.
	seed := []session.ExecProfile{
		{Name: "bare-metal", Target: session.TargetHost, Default: true},
	}
	if cfg.SandboxImage != "" {
		seed = append(seed,
			session.ExecProfile{
				Name: "sandbox", Target: session.TargetSandbox, Image: cfg.SandboxImage,
				HomeMount: os.Getenv("HOME"), Mounts: cfg.SandboxMounts, RunArgs: cfg.SandboxRunArgs,
			},
			session.ExecProfile{
				Name: "locked", Target: session.TargetSandbox, Image: cfg.SandboxImage,
				RunArgs: cfg.SandboxRunArgs,
			},
		)
	}
	profiles, err := session.OpenProfileStore(cfg.ProfilesPath, seed)
	if err != nil {
		log.Fatalf("execution profiles: %v", err)
	}
	driver.Profiles = profiles
	driver.Home = os.Getenv("HOME")
	driver.GlobalVars = cfg.ProfileVars
	log.Printf("execution profiles loaded: %d profile(s)", len(profiles.List()))
	if len(cfg.SpawnRoots) > 0 {
		// The account-global /usage check runs claude in this dir; use a spawn root so
		// it lands somewhere sane (rather than /tmp).
		driver.UsageDir = cfg.SpawnRoots[0]
	}
	driver.HostBin(cfg.ClaudeBin)
	// Per-agent binaries: Claude uses each executor's own Bin (host set above,
	// sandbox below); other backends launch their own command per target. Host turns
	// always run over SSH (the SSH executor replaces the host one below), so the host
	// target's codex binary is the remote one — mirroring how SSHExecutor.Bin carries
	// SPAWNER_SSH_CLAUDE_BIN for Claude.
	hostCodexBin := cfg.SSHCodexBin
	hostOpencodeBin := cfg.SSHOpencodeBin
	driver.AgentBins = map[string]map[session.Target]string{
		"codex": {
			session.TargetHost:    hostCodexBin,
			session.TargetSandbox: cfg.SandboxCodexBin,
		},
		"opencode": {
			session.TargetHost:    hostOpencodeBin,
			session.TargetSandbox: cfg.SandboxOpencodeBin,
		},
	}
	// SSH-native execution is unconditional: every host-target turn runs over SSH —
	// including the local machine (Session.Host empty = loopback), so there's no
	// special-cased local fork path in the running server. A bad SSH config is fatal;
	// the server never silently falls back to a direct fork.
	//
	// The server owns its OWN SSH keypair, separate from the host's ~/.ssh keys.
	// SPAWNER_SSH_KEY overrides the path; empty means self-manage under the state dir
	// (persisted on the volume), minting the key on first boot. The public key is
	// logged and written to <key>.pub — install it in the target host's
	// ~/.ssh/authorized_keys to let the server SSH in (host turns + restart button).
	selfKey := cfg.SSHKey
	if selfKey == "" {
		selfKey = filepath.Join(filepath.Dir(cfg.StatePath), "ssh", "id_ed25519")
	}
	pubLine, kerr := session.EnsureServerKey(selfKey)
	if kerr != nil {
		log.Fatalf("ssh: server key: %v", kerr)
	}
	log.Printf("server SSH public key (add to the host user's ~/.ssh/authorized_keys to enable loopback turns + the restart button):\n  %s", pubLine)
	sshConns, err := session.NewSSHPool(session.SSHConfig{
		User:       cfg.SSHUser,
		Port:       cfg.SSHPort,
		KeyFile:    selfKey,
		KnownHosts: cfg.SSHKnownHosts,
		Bin:        cfg.SSHClaudeBin,
	}, hostStore, idStore)
	if err != nil {
		log.Fatalf("ssh: %v", err)
	}
	driver.Execs[session.TargetHost] = session.SSHExecutor{Pool: sshConns}
	log.Printf("SSH-native execution: host turns run over SSH (loopback for local sessions)")
	// Auto-seed loopback into known_hosts (trust-on-first-use) so no manual
	// ssh-keyscan is needed — the server comes up bare. Best-effort: if the host
	// sshd isn't reachable yet, log and carry on (re-trusts on the next host save).
	if terr := sshConns.TrustHost(session.LocalHost, cfg.SSHPort); terr != nil {
		log.Printf("loopback host key not recorded yet (%v) — trusted automatically once localhost:22 is reachable", terr)
	} else {
		log.Printf("trusted loopback host key (%s)", session.LocalHost)
	}
	// Prime each backend's live model catalogue from the host before validating the
	// provider overlay below, so a backend that discovers its models (opencode →
	// Ollama) presents its real list and the overlay validates against it. Bounded
	// and best-effort: an unreachable host or a slow probe just leaves the compiled
	// fallback list in place (see Driver.RefreshModels). The gateway refreshes again
	// on client connect, so models added while the server runs appear without a boot.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		driver.RefreshModels(ctx)
	}()
	providers, err := agent.OpenSettingsStore(cfg.ProvidersPath, driver.Registry())
	if err != nil {
		log.Fatalf("provider settings: %v", err)
	}
	driver.Providers = providers
	if cfg.SandboxImage != "" {
		// SSH-native: a containerized server has no runtime of its own, so it drives
		// rootless podman on the host over the same pool it runs host turns on. All
		// sandbox mount/dir paths are host paths, and the sandbox's transcript is read
		// back over SSH on the runtime host (Host below) — never off the server's own
		// filesystem. HomeMount stays the container's own $HOME — set it to match the
		// host user's home in this deployment (or configure host mounts via
		// SPAWNER_SANDBOX_MOUNTS).
		sandbox := session.SandboxExecutor{
			Runtime:   cfg.SandboxRuntime,
			Image:     cfg.SandboxImage,
			Bin:       cfg.SandboxClaudeBin,
			Mounts:    cfg.SandboxMounts,
			RunArgs:   cfg.SandboxRunArgs,
			HomeMount: os.Getenv("HOME"),
			Pool:      sshConns,
			Host:      session.LocalHost,
		}
		log.Printf("sandbox turns run over SSH on %s", session.LocalHost)
		driver.Execs[session.TargetSandbox] = sandbox
		log.Printf("sandbox target enabled: %s image %q", cfg.SandboxRuntime, cfg.SandboxImage)
	}
	if driver.SandboxEnabled() {
		// Sweep sandbox containers left orphaned by sessions deleted while the server
		// was down (live ones are recreated on demand by Ensure-before-turn). Run it in
		// the background with a bounded timeout so a slow/hung container runtime can
		// never delay or wedge startup — orphan cleanup isn't boot-critical.
		known := map[string]bool{}
		for _, s := range store.List() {
			if s.Container != "" {
				known[s.Container] = true
			}
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if removed, err := driver.ReconcileContainers(ctx, known); err != nil {
				log.Printf("sandbox reconcile: %v", err)
			} else if len(removed) > 0 {
				log.Printf("sandbox reconcile: removed %d orphan container(s): %v", len(removed), removed)
			}
		}()
	}

	tmuxMgr := tmux.NewManager()

	var stt transcribe.Transcriber
	if cfg.WhisperURL != "" {
		stt = &transcribe.RemoteWhisper{URL: cfg.WhisperURL, Client: &http.Client{Timeout: 120 * time.Second}}
		log.Printf("transcription: remote whisper server at %s", cfg.WhisperURL)
	} else if cfg.WhisperModel != "" {
		stt = &transcribe.WhisperCPP{
			Bin:            cfg.WhisperBin,
			Model:          cfg.WhisperModel,
			FastModel:      cfg.WhisperModelFast,
			BaseModel:      cfg.WhisperModelBase,
			FastMaxSeconds: cfg.WhisperFastMaxSeconds,
			Lang:           cfg.WhisperLang,
		}
		if cfg.WhisperModelFast != "" {
			log.Printf("transcription: whisper.cpp (%s); fast<=%.1fs %s, else %s",
				cfg.WhisperBin, cfg.WhisperFastMaxSeconds, cfg.WhisperModelFast, cfg.WhisperModel)
		} else {
			log.Printf("transcription: whisper.cpp (%s, model %s)", cfg.WhisperBin, cfg.WhisperModel)
		}
	} else {
		log.Printf("transcription: DISABLED (set SPAWNER_WHISPER_MODEL); audio frames rejected, text utterances still work")
	}

	// Server-side speech synthesis (Kokoro): the gateway serves client `speak`
	// requests from it (nil = disabled, clients keep their on-device TTS). The
	// health check is async so a slow/absent Kokoro doesn't delay startup.
	var kokoro *tts.Client
	if cfg.TTSURL != "" {
		kokoro = tts.New(cfg.TTSURL, cfg.TTSVoice, cfg.TTSFormat)
		go func() {
			voices, err := kokoro.Voices(context.Background())
			if err != nil {
				log.Printf("tts: kokoro server at %s not answering: %v", cfg.TTSURL, err)
				return
			}
			log.Printf("tts: kokoro at %s (%d voices; default %s, format %s)",
				cfg.TTSURL, len(voices), cfg.TTSVoice, cfg.TTSFormat)
		}()
	} else {
		log.Printf("tts: DISABLED (set SPAWNER_TTS_URL); clients use on-device speech")
	}

	proj := projects.New(cfg.SpawnRoots)
	log.Printf("project index: %d dirs under roots %v", len(proj.List(1<<30)), cfg.SpawnRoots)

	gw := gateway.New(cfg, store, hostStore, idStore, sshConns, driver, tmuxMgr, stt, kokoro, proj)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", gw.HandleWS)

	// Serve the built web-client bundle (Compose/Wasm) at "/" when configured, so
	// one binary hosts both the gateway and the browser client. "/ws" and "/healthz"
	// are more specific patterns, so they still take precedence over this catch-all.
	// The static assets are public (JS/Wasm); the sensitive surface stays behind the
	// token-authenticated "/ws" handshake.
	if cfg.WebDir != "" {
		mux.Handle("/", http.FileServer(http.Dir(cfg.WebDir)))
		log.Printf("serving web client from %s at /", cfg.WebDir)
	}

	tlsConf, err := cfg.BuildTLSConfig()
	if err != nil {
		log.Fatalf("tls: %v", err)
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsConf,
	}

	go func() {
		known := store.List()
		scheme := "ws"
		if cfg.TLSEnabled() {
			scheme = "wss"
			if cfg.MutualTLS() {
				scheme = "wss+mTLS"
			}
		}
		log.Printf("spawner listening on %s [%s] (roots: %v, %d known session(s))",
			cfg.Addr, scheme, cfg.SpawnRoots, len(known))
		var err error
		if cfg.TLSEnabled() {
			// Certs are also referenced by TLSConfig-less path; passing the files
			// here lets ListenAndServeTLS load them into the (mTLS) TLSConfig.
			err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	// Shut down on a signal: Ctrl-C, `docker stop`, or the container being recreated
	// by the restart command (which rebuilds the image and recreates the container
	// from the host — see the `restart` command and Driver.Restart).
	<-stop
	log.Println("shutting down...")
	gw.NotifyShutdown() // tell connected apps their in-flight turn was interrupted
	if sshConns != nil {
		_ = sshConns.Close() // tear down pooled SSH connections + their keepalives
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutdownCtx)
	shutdownCancel()
}

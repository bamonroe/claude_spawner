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
	"syscall"
	"time"

	"github.com/bam/claude_spawner/server/internal/config"
	"github.com/bam/claude_spawner/server/internal/gateway"
	"github.com/bam/claude_spawner/server/internal/projects"
	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/tmux"
	"github.com/bam/claude_spawner/server/internal/transcribe"
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
	if len(cfg.SpawnRoots) > 0 {
		// The account-global /usage check runs claude in this dir; use a spawn root so
		// it lands somewhere sane (rather than /tmp).
		driver.UsageDir = cfg.SpawnRoots[0]
	}
	driver.HostBin(cfg.ClaudeBin)
	// SSH-native execution: when enabled, host-target turns run over SSH — every host
	// including the local machine (Session.Host empty = loopback), so there's no
	// special-cased local fork path. Transitional: off keeps the direct-fork host
	// executor. A bad SSH config is fatal (the operator explicitly opted in; don't
	// silently fall back).
	var sshConns *session.SSHPool
	if cfg.SSHEnable {
		sshConns, err = session.NewSSHPool(session.SSHConfig{
			User:       cfg.SSHUser,
			Port:       cfg.SSHPort,
			KeyFile:    cfg.SSHKey,
			KnownHosts: cfg.SSHKnownHosts,
			Bin:        cfg.SSHClaudeBin,
		}, hostStore, idStore)
		if err != nil {
			log.Fatalf("ssh: %v", err)
		}
		driver.Execs[session.TargetHost] = session.SSHExecutor{Pool: sshConns}
		log.Printf("SSH-native execution enabled: host turns run over SSH (loopback for local sessions)")
	}
	if cfg.SandboxImage != "" {
		driver.Execs[session.TargetSandbox] = session.SandboxExecutor{
			Runtime:   cfg.SandboxRuntime,
			Image:     cfg.SandboxImage,
			Bin:       cfg.SandboxClaudeBin,
			Mounts:    cfg.SandboxMounts,
			RunArgs:   cfg.SandboxRunArgs,
			HomeMount: os.Getenv("HOME"),
		}
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

	proj := projects.New(cfg.SpawnRoots)
	log.Printf("project index: %d dirs under roots %v", len(proj.List(1<<30)), cfg.SpawnRoots)

	gw := gateway.New(cfg, store, hostStore, idStore, driver, tmuxMgr, stt, proj)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", gw.HandleWS)

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
	// Shut down on a signal: Ctrl-C, `systemctl --user stop`, or the process being
	// replaced by the restart command (which rebuilds the binary and restarts the
	// unit from the host — see the `restart` command and Driver.Restart).
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

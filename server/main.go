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
	driver := session.NewDriver()
	if len(cfg.SpawnRoots) > 0 {
		// The account-global /usage check runs claude in this dir; in broker mode the
		// broker jails the cwd to SPAWNER_ROOT, so it must be an allowed root (not /tmp).
		driver.UsageDir = cfg.SpawnRoots[0]
	}
	if cfg.BrokerSocket != "" {
		// Containerized server: run turns through the host-side broker (this process
		// stays unprivileged) instead of executing locally. The broker is the single
		// host-side agent for BOTH targets — it forks claude for host turns and drives
		// the runtime for sandbox turns — so the same client backs both, and the
		// server needs neither host root nor a container-runtime socket. SANDBOX_IMAGE
		// here just enables offering sandbox; the broker owns the real runtime config.
		client := session.BrokerExecutor{Socket: cfg.BrokerSocket}
		driver.Execs[session.TargetHost] = client
		log.Printf("host turns via broker socket %s", cfg.BrokerSocket)
		if cfg.SandboxImage != "" {
			driver.Execs[session.TargetSandbox] = client
			log.Printf("sandbox turns via broker socket %s", cfg.BrokerSocket)
		}
	} else {
		driver.HostBin(cfg.ClaudeBin)
		if cfg.SandboxImage != "" {
			driver.Execs[session.TargetSandbox] = session.SandboxExecutor{
				Runtime: cfg.SandboxRuntime,
				Image:   cfg.SandboxImage,
				Bin:     cfg.SandboxClaudeBin,
				Mounts:  cfg.SandboxMounts,
				RunArgs: cfg.SandboxRunArgs,
			}
			log.Printf("sandbox target enabled: %s image %q", cfg.SandboxRuntime, cfg.SandboxImage)
		}
	}
	if driver.SandboxEnabled() {
		// Sweep sandbox containers left orphaned by sessions deleted while the server
		// was down (live ones are recreated on demand by Ensure-before-turn). Routes
		// through the broker in broker mode, or the local runtime otherwise. Run it in
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

	gw := gateway.New(cfg, store, driver, tmuxMgr, stt, proj)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/ws", gw.HandleWS)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		known := store.List()
		log.Printf("spawner listening on %s (roots: %v, %d known session(s))",
			cfg.Addr, cfg.SpawnRoots, len(known))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	// Shut down on a signal (systemctl stop / Ctrl-C → clean exit) or on a
	// client-requested restart (→ non-zero exit so the supervisor relaunches us).
	restart := false
	select {
	case <-stop:
		log.Println("shutting down...")
	case <-gw.RestartRequested():
		log.Println("restart requested by a client; exiting for the supervisor to rebuild and relaunch")
		restart = true
	}
	gw.NotifyShutdown() // tell connected apps their in-flight turn was interrupted
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = srv.Shutdown(shutdownCtx)
	shutdownCancel()
	if restart {
		// Exit non-zero so systemd's `Restart=on-failure` fires (its ExecStartPre
		// rebuilds current code before relaunch). A clean exit would instead leave
		// the unit stopped. Under `docker`/`go run` there's no supervisor, so the
		// process simply exits — restart is a systemd-deployment feature.
		os.Exit(1)
	}
}

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

	store, err := session.OpenStore(cfg.StatePath)
	if err != nil {
		log.Fatalf("session store: %v", err)
	}
	driver := session.NewDriver()
	driver.Bin = cfg.ClaudeBin

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
	<-stop
	log.Println("shutting down...")
	gw.NotifyShutdown() // tell connected apps their in-flight turn was interrupted
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

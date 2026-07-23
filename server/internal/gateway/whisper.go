package gateway

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
	"github.com/bam/claude_spawner/server/internal/transcribe"
)

// lastRateLimit returns the most recent subscription usage-window state (empty
// Type until a turn has reported one).
func (s *Server) lastRateLimit() session.RateLimit {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	return s.rateLimit
}

// setRateLimit caches the latest rate-limit state seen on a turn, so a freshly
// connected app can be shown the plan's session limit without dictating first.

// setRateLimit caches the latest rate-limit state seen on a turn, so a freshly
// connected app can be shown the plan's session limit without dictating first.
func (s *Server) setRateLimit(rl session.RateLimit) {
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	s.rateLimit = rl
}

// currentWhisperModels returns the resident servers' model names — accurate
// ("full") and fast ("quick"); the fast name is "" when no fast server is
// configured. Server-global state that apps read on connect.

// currentWhisperModels returns the resident servers' model names — accurate
// ("full") and fast ("quick"); the fast name is "" when no fast server is
// configured. Server-global state that apps read on connect.
func (s *Server) currentWhisperModels() (model, fastModel string) {
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	return s.currentModel, s.currentFast
}

// availableWhisperModels lists the ggml model names in cfg.WhisperModelsDir —
// the host directory the resident whisper servers mount at /models — sorted by
// file size (tiny → large), so clients can offer a picker instead of free text.
// Returns nil when the dir isn't configured (or can't be read); re-scanned on
// every call so dropping a new model file in needs no restart.

// availableWhisperModels lists the ggml model names in cfg.WhisperModelsDir —
// the host directory the resident whisper servers mount at /models — sorted by
// file size (tiny → large), so clients can offer a picker instead of free text.
// Returns nil when the dir isn't configured (or can't be read); re-scanned on
// every call so dropping a new model file in needs no restart.
func (s *Server) availableWhisperModels() []string {
	if s.cfg.WhisperModelsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.cfg.WhisperModelsDir)
	if err != nil {
		log.Printf("whisper: list models in %s: %v", s.cfg.WhisperModelsDir, err)
		return nil
	}
	type m struct {
		name string
		size int64
	}
	var models []m
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "ggml-") || !strings.HasSuffix(name, ".bin") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		models = append(models, m{strings.TrimSuffix(strings.TrimPrefix(name, "ggml-"), ".bin"), info.Size()})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].size < models[j].size })
	names := make([]string, len(models))
	for i, mo := range models {
		names[i] = mo.name
	}
	return names
}

// catalogWhisperModels is what the picker offers: the full curated English
// catalog (small→large) followed by any extra ggml file on disk that isn't in
// it, so the app can present every downloadable English model, not just the ones
// already fetched. Returns nil when SPAWNER_WHISPER_MODELS_DIR isn't set — with
// no dir we can't download, so the app falls back to free-text entry.

// catalogWhisperModels is what the picker offers: the full curated English
// catalog (small→large) followed by any extra ggml file on disk that isn't in
// it, so the app can present every downloadable English model, not just the ones
// already fetched. Returns nil when SPAWNER_WHISPER_MODELS_DIR isn't set — with
// no dir we can't download, so the app falls back to free-text entry.
func (s *Server) catalogWhisperModels() []string {
	if s.cfg.WhisperModelsDir == "" {
		return nil
	}
	seen := map[string]bool{}
	var names []string
	for _, m := range transcribe.EnglishModels {
		names = append(names, m.Name)
		seen[m.Name] = true
	}
	for _, n := range s.availableWhisperModels() { // on-disk, size-ordered
		if !seen[n] {
			names = append(names, n)
			seen[n] = true
		}
	}
	return names
}

// ensureModel makes sure ggml-<name>.bin is present in the models dir before a
// /load, downloading it from the catalog when it's missing. It broadcasts
// progress so the app can show a download bar (a big model is a slow fetch), and
// single-flights per name so two clients selecting the same missing model share
// one download. A no-op when the file exists, the dir is unset (free-text mode),
// or the name isn't a known catalog model (let the /load surface the error).

// ensureModel makes sure ggml-<name>.bin is present in the models dir before a
// /load, downloading it from the catalog when it's missing. It broadcasts
// progress so the app can show a download bar (a big model is a slow fetch), and
// single-flights per name so two clients selecting the same missing model share
// one download. A no-op when the file exists, the dir is unset (free-text mode),
// or the name isn't a known catalog model (let the /load surface the error).
func (s *Server) ensureModel(name string, fast bool) error {
	dir := s.cfg.WhisperModelsDir
	if dir == "" || !transcribe.IsCatalogModel(name) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, transcribe.ModelFileName(name))); err == nil {
		return nil // already present
	}
	s.downloadMu.Lock()
	if s.downloading[name] {
		s.downloadMu.Unlock()
		return fmt.Errorf("model %s is already downloading", name)
	}
	s.downloading[name] = true
	s.downloadMu.Unlock()
	defer func() {
		s.downloadMu.Lock()
		delete(s.downloading, name)
		s.downloadMu.Unlock()
	}()

	log.Printf("whisper: downloading model %s into %s", name, dir)
	s.broadcastWhisperDownload(name, fast, 0, 0, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	err := transcribe.DownloadModel(ctx, dir, name, func(received, total int64) {
		s.broadcastWhisperDownload(name, fast, received, total, false, "")
	})
	if err != nil {
		s.broadcastWhisperDownload(name, fast, 0, 0, true, err.Error())
		return err
	}
	s.broadcastWhisperDownload(name, fast, 0, 0, true, "")
	return nil
}

// broadcastWhisperDownload pushes model-download progress to every connected app.

// broadcastWhisperDownload pushes model-download progress to every connected app.
func (s *Server) broadcastWhisperDownload(name string, fast bool, received, total int64, done bool, errStr string) {
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(msgWhisperDownload(name, fast, received, total, done, errStr))
	}
}

// setWhisperModel hot-loads `name` onto a resident whisper server — the fast
// (draft/detection) one when fast is set, else the accurate one — and records
// it as that server's current model. Blocks on the /load; call it from a
// goroutine. name maps to /models/ggml-<name>.bin.

// setWhisperModel hot-loads `name` onto a resident whisper server — the fast
// (draft/detection) one when fast is set, else the accurate one — and records
// it as that server's current model. Blocks on the /load; call it from a
// goroutine. name maps to /models/ggml-<name>.bin.
func (s *Server) setWhisperModel(name string, fast bool) error {
	url := s.cfg.WhisperURL
	if fast {
		url = s.cfg.WhisperFastURL
	}
	if url == "" {
		if fast {
			return fmt.Errorf("no fast whisper server configured")
		}
		return fmt.Errorf("no resident whisper server configured")
	}
	if !validModelName(name) {
		return fmt.Errorf("invalid model name %q", name)
	}
	s.whisperMu.Lock()
	defer s.whisperMu.Unlock()
	loaded, current := &s.whisperLoaded, &s.currentModel
	if fast {
		loaded, current = &s.fastLoaded, &s.currentFast
	}
	key := url + "|" + name
	if *loaded == key {
		*current = name
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := transcribe.LoadRemoteModel(ctx, url, "/models/ggml-"+name+".bin"); err != nil {
		return fmt.Errorf("load %s: %w", name, err)
	}
	*loaded = key
	*current = name
	if fast {
		log.Printf("whisper: fast model -> %s", name)
	} else {
		log.Printf("whisper: model -> %s", name)
	}
	return nil
}

// broadcastWhisperModel tells every connected app the current resident models
// (accurate + fast), so a change made by one client updates all of them.

// broadcastWhisperModel tells every connected app the current resident models
// (accurate + fast), so a change made by one client updates all of them.
func (s *Server) broadcastWhisperModel() {
	model, fastModel := s.currentWhisperModels()
	all := s.catalogWhisperModels()
	local := s.availableWhisperModels()
	s.connsMu.Lock()
	cs := make([]*conn, 0, len(s.conns))
	for c := range s.conns {
		cs = append(cs, c)
	}
	s.connsMu.Unlock()
	for _, c := range cs {
		c.send(msgWhisperModel(model, fastModel, all, local))
	}
}

// validModelName guards the model path against injection (letters, digits, dot,
// dash, underscore only).

// validModelName guards the model path against injection (letters, digits, dot,
// dash, underscore only).
func validModelName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// clientState is what we stash when a connection drops, to resume on reconnect:
// an in-progress dialog. (Re-attaching to a session is client-driven — the app
// persists the session name and re-sends `attach`, which also survives a server
// restart because sessions are durable on disk.)

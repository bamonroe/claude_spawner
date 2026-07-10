package gateway

import (
	"log"
	"time"
)

// Auto-compress: when enabled, the server watches every started session and, once
// its context grows past the token threshold, fires a compress in the last seconds
// of the prompt cache's warm window — so the summary turn reuses the still-warm
// cache (a cheap cache_read) instead of letting the cache go cold and paying a full
// context rebuild on the next dictation. The config is a global user preference the
// app sets over the wire (in `hello` and via the live `auto_compress` message); the
// trigger itself is server-owned so it still fires after the app detaches.

// warmCacheWindow is Anthropic's prompt-cache TTL: a turn started within this of the
// previous turn reuses the warm cache. Mirrors the app's CacheWarmBar countdown.
const warmCacheWindow = 5 * time.Minute

// autoCompressLead fires the compress this long before the warm window expires,
// leaving the summary turn enough of the window to still land on the warm cache.
const autoCompressLead = 15 * time.Second

// autoCompressTick is how often the monitor re-scans every session.
const autoCompressTick = 5 * time.Second

// autoCompressCfg is the global auto-compress preference (all sessions/clients).
type autoCompressCfg struct {
	enabled    bool
	thresholdK int // context-token threshold, in thousands
}

// setAutoCompress records the global preference (from `hello` or a live
// `auto_compress` message). A non-positive threshold is treated as disabled.
func (s *Server) setAutoCompress(enabled bool, thresholdK int) {
	s.autoCompressMu.Lock()
	defer s.autoCompressMu.Unlock()
	s.acCfg = autoCompressCfg{enabled: enabled && thresholdK > 0, thresholdK: thresholdK}
}

func (s *Server) autoCompressConfig() autoCompressCfg {
	s.autoCompressMu.Lock()
	defer s.autoCompressMu.Unlock()
	return s.acCfg
}

// firedAutoCompress reports whether we've already auto-compressed the turn stamped
// `at` for this session (recording it if not), so the ~15s trigger window doesn't
// fire repeatedly while it stays open.
func (s *Server) firedAutoCompress(sessionID string, at int64) bool {
	s.autoCompressMu.Lock()
	defer s.autoCompressMu.Unlock()
	if s.acFired[sessionID] == at {
		return true
	}
	s.acFired[sessionID] = at
	return false
}

// autoCompressLoop is the server-owned monitor: every tick it scans started
// sessions and compresses any that are over the token threshold and within the
// last autoCompressLead of their warm-cache window. Started once from New().
func (s *Server) autoCompressLoop() {
	t := time.NewTicker(autoCompressTick)
	defer t.Stop()
	for range t.C {
		cfg := s.autoCompressConfig()
		if !cfg.enabled {
			continue
		}
		now := time.Now()
		for _, sess := range s.store.List() {
			if !sess.Started || s.isBusy(sess.SessionID) {
				continue // nothing to compress, or a turn is already running
			}
			cx := s.driver.LastContextUsage(sess.Agent, sess.Host, sess.TranscriptIDs())
			if cx == nil || cx.At == 0 {
				continue
			}
			// Context size, measured the same way the app's badge does: input + both
			// cache halves = the whole prompt that would be re-read on a cold turn.
			ctxTokens := cx.Usage.Input + cx.Usage.CacheRead + cx.Usage.CacheWrite
			if ctxTokens < cfg.thresholdK*1000 {
				continue
			}
			remaining := warmCacheWindow - now.Sub(time.Unix(cx.At, 0))
			if remaining > autoCompressLead || remaining <= 0 {
				continue // not yet in the trigger window, or the cache is already cold
			}
			if s.firedAutoCompress(sess.SessionID, cx.At) {
				continue
			}
			log.Printf("auto-compress[%s]: %d ctx tokens ≥ %dk, %ds to cache expiry — compressing",
				sess.Name, ctxTokens, cfg.thresholdK, int(remaining.Seconds()))
			s.startCompress(sess)
		}
	}
}

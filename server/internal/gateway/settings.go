package gateway

import (
	"errors"
	"strconv"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The shared-settings catalogue (the fifth app-managed catalogue) routes genuinely-
// shared server-global scalars — the resident whisper models, the auto-compress
// config, and summary-only speech — through the same versioned sync machinery as
// hosts/identities/profiles/providers: keyed {key, value, updated_at} records,
// per-key last-writer-wins, and an order-independent digest exchanged in `hello`.
// The app is the source of truth on write (it stamps updated_at); the server persists
// each record and re-broadcasts the `settings` list on any change. Per-device
// preferences (wake sensitivity, on-device vs server TTS, color scheme) are NOT here
// — they stay local to each client.

// settingKeys is the fixed set of shared-setting keys this catalogue carries. A
// setting_put naming anything else is rejected, so a typo can't silently persist a
// junk record that would then diverge the digest.
var settingKeys = map[string]bool{
	"whisper_model":           true,
	"whisper_fast_model":      true,
	"warm_compress":           true,
	"auto_compress":           true,
	"auto_compress_threshold": true,
	"summary_only":            true,
}

// broadcastSettings re-sends the whole shared-settings catalogue to every client, so
// a change made by one client (or server-side, e.g. a whisper load completing) lands
// on all connected apps at once.
func (s *Server) broadcastSettings() {
	s.broadcast(msgSettings(s.settings.List()))
}

// applyAutoCompressFromStore recomputes the live auto-compress preference from the
// three persisted records and pushes it to the server-owned monitor. Called at boot
// and after any auto-compress setting changes, so the store is the single source of
// truth for the trigger config.
func (s *Server) applyAutoCompressFromStore() {
	warm := s.settings.Value("warm_compress") == "true"
	auto := s.settings.Value("auto_compress") == "true"
	thresholdK, _ := strconv.Atoi(s.settings.Value("auto_compress_threshold"))
	s.setAutoCompress(warm, auto, thresholdK)
}

// persistSetting upserts a shared-setting record stamped with the given time and
// returns whether the write landed (false = a newer record already won, so the
// caller should re-sync the stale client instead of broadcasting). A real error is
// logged and treated as "landed" (best-effort persistence, live state already set).
func (s *Server) persistSetting(key, value string, updatedAt int64) bool {
	err := s.settings.Put(&session.SettingRecord{Key: key, Value: value, UpdatedAt: updatedAt})
	if errors.Is(err, session.ErrStale) {
		return false
	}
	return true
}

// doSettingPut stores an app-originated shared-setting change (last-writer-wins),
// applies its live effect, and broadcasts the updated catalogue. A stale write (an
// older updated_at) re-syncs just this client with the newer record instead.
func (c *conn) doSettingPut(key, value string, updatedAt int64) {
	if !settingKeys[key] {
		c.fail("bad_setting", "unknown setting key: "+key)
		return
	}
	if err := c.srv.settings.Put(&session.SettingRecord{Key: key, Value: value, UpdatedAt: updatedAt}); err != nil {
		if errors.Is(err, session.ErrStale) {
			c.send(msgSettings(c.srv.settings.List()))
			return
		}
		c.fail("internal", err.Error())
		return
	}
	// Apply the live server-side effect of the newly-stored value. The whisper models
	// have their own load path (set_whisper_model), so a whisper key arriving here just
	// updates the stored/synced value; the actual resident-server reload rides that
	// dedicated message. Auto-compress reconfigures the server-owned monitor.
	switch key {
	case "warm_compress", "auto_compress", "auto_compress_threshold":
		c.srv.applyAutoCompressFromStore()
	}
	c.srv.broadcastSettings()
}

// nowMillis is the server-side last-edit stamp for a server-originated setting change
// (a whisper load completing, the summary-only voice command), matching the app's
// client-stamped unix-millisecond scale so last-writer-wins compares like-for-like.
func nowMillis() int64 { return time.Now().UnixMilli() }

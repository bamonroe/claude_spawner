package gateway

import (
	"crypto/subtle"
	"strings"
	"time"
)

// authenticate requires the first message to be a valid hello.
func (c *conn) authenticate() bool {
	_ = c.ws.SetReadDeadline(time.Now().Add(handshakeTimeout))
	var in inbound
	if err := c.ws.ReadJSON(&in); err != nil {
		return false
	}
	_ = c.ws.SetReadDeadline(time.Time{}) // clear deadline
	if in.Type != "hello" || subtle.ConstantTimeCompare([]byte(in.Token), []byte(c.srv.cfg.AuthToken)) != 1 {
		c.fail("unauthorized", "bad or missing token")
		return false
	}
	c.clientID = in.ClientID
	c.brief = in.Brief
	c.interactive = in.Interactive
	// Auto-compress is no longer clobbered from hello — it's a synced setting now,
	// reconciled below via the settings digest / last-writer-wins, so a fresh client
	// doesn't stomp a preference another client set. See the settings catalogue block.
	// Wake, end and speak phrases now come from the server-wide spoken-token
	// catalogue (c.wakePhrases/endPhrases/speakPhrases), not per-hello flat fields —
	// so all clients share one configurable set. The flat in.EndToken/WakeToken/
	// SpeakToken hints from older apps are ignored. Only the per-device gate toggle
	// and detector-service choice stay per connection.
	c.dictationGate = in.DictationGate
	c.sttMode = in.SttMode
	c.sttModel = in.SttModel
	// Live wake/end-token backend. Default to the always-present Whisper string-match
	// so a fresh/older client never lands on the trained sidecar implicitly — the
	// detector is opt-in per client (the app's toggle), even when SPAWNER_WAKEWORD_URL
	// is configured server-wide. Anything other than "detector" means Whisper.
	c.wakeService = strings.TrimSpace(in.WakeService)
	c.aliases = in.Aliases
	// The whisper model is server-global: the app reads it here rather than pushing
	// its own (so two clients don't bounce it), and changes it via set_whisper_model.
	model, fastModel := c.srv.currentWhisperModels()
	c.send(msgHelloOK("ws", model, fastModel, c.srv.catalogWhisperModels(), c.srv.availableWhisperModels(), c.srv.tts != nil))
	// Per-catalogue digest fast path (skip-if-equal): the app presents a digest of
	// each cached catalogue in `hello`; we re-send only the ones whose digest differs
	// from ours, so an unchanged catalogue costs nothing on connect (Phase 2a LWW
	// resolves direction on the ones we do send). Mirrors the chat transcript's
	// count+hash `digests`/`history unchanged` shortcut. An older client sends no
	// digest → the "" mismatch makes every catalogue ship, so it's fully backward
	// compatible. See catalogdigest.go.
	//
	// Advertise the AI backend registry so the app's new-session picker can offer a
	// backend + model choice (and badge sessions by backend). Also kick a throttled
	// live re-discovery in the background: models added to a backend since boot land
	// in every connected app moments later, no restart needed.
	reg := c.srv.driver.Registry()
	provSettings := c.srv.driver.ProviderSettings()
	if in.ProvidersDigest != providersDigest(reg, provSettings) {
		c.send(msgAgents(reg, provSettings))
	}
	go c.srv.refreshModelsOnConnect()
	// Advertise execution profiles separately from hello_ok so older clients can
	// ignore the message and still use the built-in default profile.
	profReg := c.srv.driver.ProfileRegistry()
	if in.ProfilesDigest != profilesDigest(profReg.List()) {
		c.send(msgProfiles(profReg))
	}
	// Advertise the closed action set (compile-time) unconditionally, then re-send the
	// spoken-token catalogue only when the app's digest differs (skip-if-equal fast
	// path). The app binds phrases to the advertised actions and edits the tokens.
	c.send(msgActions())
	if in.SpokenTokensDigest != spokenTokensDigest(c.srv.tokens.List()) {
		c.send(msgSpokenTokens(c.srv.tokens.List()))
	}
	// Hosts and identities were previously request-only; presenting a digest lets us
	// proactively reconcile them on connect too, so a different client's edit reflects
	// here without opening the settings screen — but only when they actually differ.
	if in.HostsDigest != hostsDigest(c.srv.hosts.List()) {
		c.send(msgHostList(c.srv.hosts.List()))
	}
	if in.IdentitiesDigest != identitiesDigest(c.srv.ids.List()) {
		c.send(msgIdentityList(c.srv.ids.List()))
	}
	// The fifth catalogue: genuinely-shared server-global scalars (whisper models,
	// auto-compress, summary-only). Same skip-if-equal fast path — re-send only when
	// the app's digest differs; LWW on the records reconciles direction on the ones
	// we do send.
	if in.SettingsDigest != settingsDigest(c.srv.settings) {
		c.send(msgSettings(c.srv.settings.List()))
	}
	// Push the last-known plan session-limit so the app can show it immediately,
	// rather than staying blank until the first turn of this connection.
	if rl := c.srv.lastRateLimit(); rl.Type != "" {
		c.send(msgRateLimit(rl))
	}
	return true
}

// restoreState re-applies any saved attach/dialog state for this client, so a
// reconnect resumes seamlessly. Runs after the hello_ok is sent.

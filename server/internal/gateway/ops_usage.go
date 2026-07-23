package gateway

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// doUsage runs `/usage` (a full but lightweight claude invocation) in the
// background and returns the plan's usage report — the session/weekly percent-
// used numbers the TUI `/usage` shows. If speak, it also sends a short spoken
// summary (the "usage" voice command); a tap trigger stays silent and just fills
// the app's usage sheet.
func (c *conn) doUsage(speak bool) {
	if speak {
		c.send(msgSay("checking your usage — one sec."))
	}
	go func() {
		ctx, cancel := context.WithTimeout(c.ctx, 90*time.Second)
		defer cancel()
		text, err := c.srv.driver.Usage(ctx)
		if err != nil {
			c.fail("usage_failed", err.Error())
			return
		}
		sp, sr, wp, wr := parseUsage(text)
		c.send(msgUsage(sp, sr, wp, wr, strings.TrimSpace(text)))
		if speak {
			c.send(msgSay(usageSummary(sp, wp)))
		}
	}()
}

var (
	reUsageSession = regexp.MustCompile(`(?i)current session:\s*(\d+)% used(?:\s*·\s*resets\s*([^\n]+))?`)
	reUsageWeek    = regexp.MustCompile(`(?i)current week \(all models\):\s*(\d+)% used(?:\s*·\s*resets\s*([^\n]+))?`)
)

// parseUsage pulls the session and weekly percent-used headline out of a /usage
// report. Returns -1 for a percent it couldn't find and "" for a missing reset;
// the app shows the full text verbatim regardless, so this is a best-effort
// headline, not the whole story.

// parseUsage pulls the session and weekly percent-used headline out of a /usage
// report. Returns -1 for a percent it couldn't find and "" for a missing reset;
// the app shows the full text verbatim regardless, so this is a best-effort
// headline, not the whole story.
func parseUsage(text string) (sessionPct int, sessionReset string, weekPct int, weekReset string) {
	sessionPct, weekPct = -1, -1
	if m := reUsageSession.FindStringSubmatch(text); m != nil {
		sessionPct = atoiSafe(m[1])
		sessionReset = cleanReset(m[2])
	}
	if m := reUsageWeek.FindStringSubmatch(text); m != nil {
		weekPct = atoiSafe(m[1])
		weekReset = cleanReset(m[2])
	}
	return
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return -1
	}
	return n
}

// cleanReset trims the reset string and drops a trailing " (timezone)" note so
// "Jul 4, 9:59am (America/New_York)" reads as "Jul 4, 9:59am".

// cleanReset trims the reset string and drops a trailing " (timezone)" note so
// "Jul 4, 9:59am (America/New_York)" reads as "Jul 4, 9:59am".
func cleanReset(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, " ("); i > 0 {
		s = s[:i]
	}
	return s
}

// usageSummary is the short spoken line for the "usage" voice command.

// usageSummary is the short spoken line for the "usage" voice command.
func usageSummary(sessionPct, weekPct int) string {
	switch {
	case sessionPct < 0 && weekPct < 0:
		return "couldn't read your usage."
	case sessionPct < 0:
		return fmt.Sprintf("you've used %d%% of this week's limit.", weekPct)
	case weekPct < 0:
		return fmt.Sprintf("you've used %d%% of this session's limit.", sessionPct)
	default:
		return fmt.Sprintf("you've used %d%% of this session and %d%% of the week.", sessionPct, weekPct)
	}
}

// doDiscover scans ~/.claude/projects for all Claude sessions (spawner-created
// or not) and returns them, flagged with whether they're already registered and
// whether an interactive claude is live in tmux at that directory.

// doListModels speaks the models the attached session's AI backend offers, in
// order, so the user can pick one by NUMBER ("use model 2"). Ordinal selection
// keeps hard-to-say model names (e.g. Codex's gpt-5.5 reasoning presets) out of
// the voice path. Marks the session's current model.
func (c *conn) doListModels() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ag := c.srv.driver.AgentFor(c.attached)
	// Only the voice-enumerable subset (per the Providers settings) is spoken and
	// numbered, so hard-to-say or hidden models stay out of the spoken flow. The
	// ordinals here must match doUseModel, which indexes the same subset.
	models := c.srv.driver.ProviderSettings().VoiceModels(ag)
	if ag == nil || len(models) == 0 {
		c.send(msgSay("this session's AI has no selectable models."))
		return
	}
	// An empty session Model means the backend's own default — mark that one.
	current := c.attached.Model
	if current == "" {
		current = c.srv.driver.ProviderSettings().DefaultModel(ag)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s has %d models. ", ag.Name, len(models))
	for i, m := range models {
		mark := ""
		if m.Alias == current {
			mark = ", current"
		}
		fmt.Fprintf(&b, "%d, %s%s. ", i+1, m.Alias, mark)
	}
	b.WriteString("say use model and a number to switch.")
	c.send(msgSay(b.String()))
}

// doUseModel switches the attached session's model to the n-th in its backend's
// catalogue (1-based, matching doListModels). Durable — persisted on the session
// and read by the next turn (a turn already in flight finishes on the old model).

// doUseModel switches the attached session's model to the n-th in its backend's
// catalogue (1-based, matching doListModels). Durable — persisted on the session
// and read by the next turn (a turn already in flight finishes on the old model).
func (c *conn) doUseModel(n int) {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	ag := c.srv.driver.AgentFor(c.attached)
	models := c.srv.driver.ProviderSettings().VoiceModels(ag)
	if ag == nil || len(models) == 0 {
		c.send(msgSay("this session's AI has no selectable models."))
		return
	}
	if n < 1 || n > len(models) {
		c.send(msgSay(fmt.Sprintf("say a model number between 1 and %d — list models to hear them.", len(models))))
		return
	}
	m := models[n-1]
	c.attached.Model = m.Alias
	if err := c.srv.store.Put(c.attached); err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.send(msgSay(fmt.Sprintf("switched to %s. it takes effect on your next message.", m.Alias)))
}

// doCompress compacts the attached session's Claude context: it asks Claude to
// summarize the conversation so far, then rotates to a fresh session_id whose
// next dictation is seeded with that summary. Unlike doClear (which drops context
// entirely), compress preserves it in condensed form — the Claude Code `/compact`
// analogue. The old transcript is kept on disk and history still spans the whole
// chain. The summary runs as a background turn (see startCompress); refused if a
// turn is in flight or no turn has run yet. Shared by the voice command and the
// app action.

// doCompress compacts the attached session's Claude context: it asks Claude to
// summarize the conversation so far, then rotates to a fresh session_id whose
// next dictation is seeded with that summary. Unlike doClear (which drops context
// entirely), compress preserves it in condensed form — the Claude Code `/compact`
// analogue. The old transcript is kept on disk and history still spans the whole
// chain. The summary runs as a background turn (see startCompress); refused if a
// turn is in flight or no turn has run yet. Shared by the voice command and the
// app action.
func (c *conn) doCompress() {
	if c.attached == nil {
		c.send(msgSay("attach to a session first."))
		return
	}
	s := c.attached
	if !s.Started {
		c.send(msgSay("nothing to compress yet."))
		return
	}
	if c.srv.isBusy(s.SessionID) {
		c.send(msgSay("still working on the last one — try compressing when it's done."))
		return
	}
	c.clearBuffer()
	if !c.srv.startCompress(s) {
		c.send(msgSay("still working on the last one."))
	}
}

// removeSession deletes a session: detaches if we're on it, drops its job, and
// pushes the refreshed list. Returns false (with an error) if unknown.

// doSetWhisperModel changes a resident whisper server's model (server-global) —
// the fast (draft/detection) server when fast is set, else the accurate one.
// The /load blocks (a big model takes seconds), so run it off the read loop; on
// success, broadcast the new models to every client, else report the error.
func (c *conn) doSetWhisperModel(name string, fast bool) {
	go func() {
		name = strings.TrimSpace(name)
		// Fetch the model first if it's a known catalog model not yet on disk; this
		// broadcasts download progress and is a no-op when it's already present.
		if err := c.srv.ensureModel(name, fast); err != nil {
			c.fail("whisper_failed", err.Error())
			return
		}
		if err := c.srv.setWhisperModel(name, fast); err != nil {
			c.fail("whisper_failed", err.Error())
			return
		}
		model, fastModel := c.srv.currentWhisperModels()
		// Persist the choice through the shared-settings catalogue so a restart/rebuild
		// keeps it and other clients sync to it. The whisper model is one of the synced
		// records; the load is server-authoritative, so stamp it with the server clock.
		key, val := "whisper_model", model
		if fast {
			key, val = "whisper_fast_model", fastModel
		}
		c.srv.persistSetting(key, val, nowMillis())
		c.srv.broadcastWhisperModel()
		c.srv.broadcastSettings()
	}()
}

// doRestart fires the configured restart command to rebuild and relaunch the
// server, picking up any new server code. The command (SPAWNER_RESTART_CMD) runs
// detached on the host — SSHing over and running deploy/rebuild-container.sh to
// rebuild the image and recreate this container — so the process is replaced out
// from under us and the app auto-reconnects once the fresh one is listening. Any authenticated client may
// trigger this; the trust boundary is the same as spawning arbitrary commands.
// Reports back if restart isn't configured (SPAWNER_RESTART_CMD unset) instead of
// pretending it worked.
// mode selects what the restart command does: "build" rebuilds the image only and
// leaves the running container in place (no bounce, so the caller's live session keeps
// going); "bounce" recreates the container from the existing image without recompiling;
// "rebuild" (the default, and empty from the voice command) builds then recreates.

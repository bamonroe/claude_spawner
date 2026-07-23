package gateway

import (
	"log"
	"path/filepath"

	"github.com/bam/claude_spawner/server/internal/session"
)

// doDiscover scans ~/.claude/projects for all Claude sessions (spawner-created
// or not) and returns them, flagged with whether they're already registered and
// whether an interactive claude is live in tmux at that directory.
func (c *conn) doDiscover() {
	found, err := c.srv.driver.DiscoverSessions("")
	if err != nil {
		c.fail("discover_failed", err.Error())
		return
	}
	active := c.srv.tmuxMgr.ClaudeDirs(c.ctx)
	registered := c.srv.store.List()
	// last-active per dir, from discovery (used only to timestamp registered rows).
	discByDir := map[string]session.Discovered{}
	for _, d := range found {
		discByDir[d.Dir] = d
	}
	views := make([]discoveredView, 0, len(registered)+len(found))
	regDirs := map[string]bool{}
	// One row per REGISTERED session, keyed by its own session_id — no directory
	// collapse, so multiple sessions in the same dir are each individually visible
	// and separately renamable/deletable (this is what stops sessions hiding).
	for _, s := range registered {
		regDirs[s.Dir] = true
		views = append(views, discoveredView{
			Name: s.Name, Dir: s.Dir, SessionID: s.SessionID,
			LastActive: discByDir[s.Dir].LastActive, Active: active[s.Dir], Registered: true,
			Busy: c.srv.isBusy(s.SessionID), Target: sandboxTarget(s), Host: s.Host,
			Agent: s.Agent, Model: s.Model, Profile: s.Profile,
		})
	}
	// Unregistered sessions found on disk — one adoptable row per directory (these
	// aren't managed yet, so a per-dir entry to offer adoption is enough).
	for _, d := range found {
		if regDirs[d.Dir] {
			continue
		}
		views = append(views, discoveredView{
			Name: sanitizeName(filepath.Base(d.Dir)), Dir: d.Dir, SessionID: d.SessionID,
			LastActive: d.LastActive, Active: active[d.Dir], Registered: false,
			// Discovery scans this machine's disk, so an unregistered find is local.
			Host: session.LocalHost,
		})
	}
	c.send(msgDiscovered(views))
}

// doRenameDiscovered gives a discovered session a custom name. It resolves the
// target by SESSION_ID — the stable identity the app sends — not by directory, so
// with several sessions in one dir the rename lands on exactly the one picked
// (resolving by dir would hit whichever record won the byDir map). Registers the
// session (without attaching) if it isn't in the store yet, then renames.

// doRenameDiscovered gives a discovered session a custom name. It resolves the
// target by SESSION_ID — the stable identity the app sends — not by directory, so
// with several sessions in one dir the rename lands on exactly the one picked
// (resolving by dir would hit whichever record won the byDir map). Registers the
// session (without attaching) if it isn't in the store yet, then renames.
func (c *conn) doRenameDiscovered(sessionID, dir, newName string) {
	if sanitizeName(newName) == "" {
		c.fail("bad_rename", "need a valid new_name")
		return
	}
	rec := c.srv.store.GetBySessionID(sessionID)
	if rec == nil {
		if dir == "" {
			c.fail("bad_rename", "need session_id or dir")
			return
		}
		var err error
		if rec, err = c.srv.registerDiscovered(sessionID, dir); err != nil {
			c.fail("internal", err.Error())
			return
		}
	}
	c.doRename(rec.Name, newName)
	c.doDiscover()
}

// doSetAgent switches a session's AI backend (and model) durably. It mirrors
// doRenameDiscovered: it locates the session by session_id (registering a still-
// discovered one by dir first), then stamps the resolved backend + a model valid
// for it (an explicit alias that isn't in the new backend's catalogue falls back to
// its default). Changing the backend rotates to a fresh session_id and un-Starts
// the session: Claude and Codex transcripts use incompatible on-disk formats, so the
// new AI can't resume the old one's history directly. Rather than drop that context,
// it reads the outgoing backend's transcript through the generic per-backend reader
// and carries a portable recap forward as PendingSeed, so the new backend continues
// the same conversation from its first turn (the old transcript stays on disk too). A
// backend whose transcript isn't readable yields an empty recap and switches clean.

// doSetAgent switches a session's AI backend (and model) durably. It mirrors
// doRenameDiscovered: it locates the session by session_id (registering a still-
// discovered one by dir first), then stamps the resolved backend + a model valid
// for it (an explicit alias that isn't in the new backend's catalogue falls back to
// its default). Changing the backend rotates to a fresh session_id and un-Starts
// the session: Claude and Codex transcripts use incompatible on-disk formats, so the
// new AI can't resume the old one's history directly. Rather than drop that context,
// it reads the outgoing backend's transcript through the generic per-backend reader
// and carries a portable recap forward as PendingSeed, so the new backend continues
// the same conversation from its first turn (the old transcript stays on disk too). A
// backend whose transcript isn't readable yields an empty recap and switches clean.
func (c *conn) doSetAgent(sessionID, dir, agentID, modelAlias string) {
	rec := c.srv.store.GetBySessionID(sessionID)
	if rec == nil {
		if dir == "" {
			c.fail("bad_agent", "need session_id or dir")
			return
		}
		var err error
		if rec, err = c.srv.registerDiscovered(sessionID, dir); err != nil {
			c.fail("internal", err.Error())
			return
		}
	}
	attachedHere := c.attached != nil && c.attached.SessionID == rec.SessionID
	ag := c.srv.driver.Registry().Resolve(agentID)
	model := c.srv.driver.ProviderSettings().DefaultModel(ag)
	if modelAlias != "" {
		if m, ok := ag.Model(modelAlias); ok {
			model = m.Alias
		}
	}
	// Compare resolved ids so an empty/omitted agent (== the default backend) is a
	// no-op against a session already on that backend and doesn't force a rotation.
	curID := c.srv.driver.Registry().Resolve(rec.Agent).ID
	var oldID string
	if curID != ag.ID {
		if c.srv.isBusy(rec.SessionID) {
			c.fail("busy", "still working — switch the agent when the turn finishes")
			return
		}
		// Carry the outgoing backend's conversation across the switch: read its
		// transcript through the generic per-backend reader and stash a portable recap
		// as the seed for the first turn on the new backend. Must run BEFORE the
		// rotation below — rec.Agent and TranscriptIDs() still point at the old chain
		// here. A backend with no readable transcript (e.g. antigravity's null reader)
		// yields an empty recap, so the switch is clean exactly as before.
		var handoffSeed string
		if msgs, err := c.srv.driver.ReadTranscriptChain(rec.Agent, rec.Host, rec.TranscriptIDs()); err != nil {
			log.Printf("set_agent[%s]: read prior transcript for handoff: %v", rec.Name, err)
		} else {
			handoffSeed = formatHandoffRecap(msgs)
		}
		// Archive the outgoing backend as a display segment so its messages stay in
		// the chat log after the rotation drops them from context (rec.Agent/host/ids
		// still point at the old backend here). Skip an un-run backend — nothing to
		// show — so repeated no-op switches don't pile up empty segments.
		if rec.Started {
			rec.History = append(rec.History, c.srv.driver.ArchiveSegment(rec))
		}
		newID, err := session.NewSessionID()
		if err != nil {
			c.fail("internal", err.Error())
			return
		}
		oldID = rec.SessionID
		rec.SessionID = newID
		rec.Started = false
		rec.AskPrimed = false
		rec.JobsPrimed = false        // re-prime the background-job instruction on the new backend
		rec.PriorIDs = nil            // don't chain the old backend's transcripts into the new one
		rec.PendingSeed = handoffSeed // ...carry a recap of them across the switch instead
	}
	rec.Agent = ag.ID
	rec.Model = model
	if err := c.srv.store.Put(rec); err != nil {
		c.fail("internal", err.Error())
		return
	}
	if oldID != "" {
		// The session_id rotated: move the hub + id index onto the new id so an
		// attached device still receives the next turn, and forget the old id.
		c.srv.rekeyJob(oldID, rec.SessionID)
		if ferr := c.srv.store.ForgetID(oldID); ferr != nil {
			log.Printf("forget rotated id %s: %v", oldID, ferr)
		}
	}
	if attachedHere {
		c.setAttached(rec)
		c.send(msgAttached(rec, nil)) // refresh the app's backend/model badge in place
	}
	c.sendSessionList()
	c.doDiscover()
}

// doAdopt registers a discovered Claude session (by session_id + dir) into the
// spawner store and attaches to it, so the app can drive/view it via --resume.
// If the session_id is already registered, it just attaches.

// doAdopt registers a discovered Claude session (by session_id + dir) into the
// spawner store and attaches to it, so the app can drive/view it via --resume.
// If the session_id is already registered, it just attaches.
func (c *conn) doAdopt(sessionID, dir string) {
	if sessionID == "" || dir == "" {
		c.fail("bad_adopt", "adopt needs session_id and dir")
		return
	}
	if s := c.srv.store.GetBySessionID(sessionID); s != nil {
		c.doAttach(s.Name, false)
		return
	}
	// A session_id is the sole identity: adopt the requested one verbatim. A folder
	// that already hosts another session is fine — the adopted session is a distinct
	// one and its name simply dedups to "<dir>-2".
	rec, err := c.srv.registerDiscovered(sessionID, dir)
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	c.sendSessionList()
	c.doAttach(rec.Name, false)
}

// doDeleteDiscovered PERMANENTLY deletes a session row. A REGISTERED row is one
// session: its transcript(s) — current session_id plus rotated prior ids — and its
// single registry record; deleting it leaves dir-mates that have their own rows.
// An UNREGISTERED row stands for a whole directory of loose transcripts (discover
// shows one adoptable row per dir), so it wipes every transcript in that dir — else
// deleting one just re-surfaces the row on a sibling. Refuses while the directory is
// live in a terminal (deleting under a running claude corrupts it). Refreshes the
// discover + session lists.

// doDeleteDiscovered PERMANENTLY deletes a session row. A REGISTERED row is one
// session: its transcript(s) — current session_id plus rotated prior ids — and its
// single registry record; deleting it leaves dir-mates that have their own rows.
// An UNREGISTERED row stands for a whole directory of loose transcripts (discover
// shows one adoptable row per dir), so it wipes every transcript in that dir — else
// deleting one just re-surfaces the row on a sibling. Refuses while the directory is
// live in a terminal (deleting under a running claude corrupts it). Refreshes the
// discover + session lists.
func (c *conn) doDeleteDiscovered(sessionID string) {
	if sessionID == "" {
		c.fail("bad_delete", "need session_id")
		return
	}
	rec := c.srv.store.GetBySessionID(sessionID)
	var dir string
	if rec != nil {
		dir = rec.Dir
	} else if p := c.srv.driver.TranscriptPathByID("", sessionID); p != "" {
		dir = c.srv.driver.TranscriptCwd("", p)
	}
	if dir == "" {
		c.fail("not_found", "no transcript or record for that session")
		return
	}
	// Guard against corrupting a transcript that a live interactive claude in this
	// directory might be writing.
	if c.srv.tmuxMgr.ClaudeDirs(c.ctx)[dir] {
		c.fail("session_active", "that session is live in a terminal — close it there first")
		return
	}
	// A registered row is one session — remove exactly its transcripts (current id
	// plus rotated prior ids), leaving any dir-mates that have their own rows. An
	// unregistered row stands for the WHOLE directory (discover collapses a dir's
	// loose transcripts into a single adoptable row), so wipe every transcript in
	// that dir — otherwise deleting one just re-surfaces the row on a dir-mate.
	var err error
	if rec != nil {
		// Backend-aware full purge across every backend the session ran: the current
		// chain (current + rotated prior ids) plus any archived cross-backend History
		// segments — transcript, sidecar, and per-session state for Claude; rollout
		// files for Codex. Leaves any dir-mates that have their own rows.
		_, err = c.srv.driver.DeleteSessionAll(rec)
	} else {
		// Unregistered rows come from the discovery scan on the loopback host
		// (TranscriptPathByID above reads the same place), so delete there.
		_, err = c.srv.driver.DeleteSessionsForDir(c.ctx, "", sessionID, dir)
	}
	if err != nil {
		c.fail("internal", err.Error())
		return
	}
	if rec != nil {
		if c.attached != nil && c.attached.SessionID == rec.SessionID {
			c.doDetach()
		}
		if derr := c.srv.store.Delete(rec.Name); derr != nil {
			log.Printf("delete session record %s: %v", rec.Name, derr)
		}
		c.removeSandbox(rec) // destroy the session's container, if any
		c.srv.dropJob(rec.SessionID)
	}
	c.sendSessionList()
	c.doDiscover()
}

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.

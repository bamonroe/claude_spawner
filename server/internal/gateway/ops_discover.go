package gateway

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

// doDiscover answers a discover request off the connection's serial read loop
// (the walk and the tmux inspection both do I/O; inline they blocked every other
// request under load). Requests coalesce per connection: one in flight at a
// time, and a request arriving meanwhile is dropped because the running one's
// `discovered` frame answers it too — same shape as startDigestSweep.
func (c *conn) doDiscover() {
	if !c.discovering.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.discovering.Store(false)
		c.serveDiscover()
	}()
}

// serveDiscover builds and sends the session list, flagged with whether each is
// already registered and whether an interactive claude is live in tmux at that
// directory. It reads the MEMOIZED walk (discoverSnapshot) rather than walking
// the disk per request, and it never hard-fails the frame: with no walk data at
// all it still emits every registered row (from the store) marked partial:true,
// so under load the app degrades to a list without adoptable rows instead of
// getting nothing.
func (c *conn) serveDiscover() {
	found, ok := c.srv.discoverSnapshot()
	active := c.srv.tmuxMgr.ClaudeDirs(c.ctx)
	registered := c.srv.store.List()
	// Last-active per session id AND per dir. Registered rows match by their OWN
	// transcript ids first — the per-dir fallback alone gave a dir-mate's (or a
	// rotated-away) session LastActive 0 or its neighbor's time, sinking or
	// shuffling it in the app's sidebar.
	lastByID := map[string]int64{}
	lastByDir := map[string]int64{}
	for _, d := range found {
		if d.LastActive > lastByID[d.SessionID] {
			lastByID[d.SessionID] = d.LastActive
		}
		if d.LastActive > lastByDir[d.Dir] {
			lastByDir[d.Dir] = d.LastActive
		}
	}
	views := make([]discoveredView, 0, len(registered)+len(found))
	regDirs := map[string]bool{}
	// One row per REGISTERED session, keyed by its own session_id — no directory
	// collapse, so multiple sessions in the same dir are each individually visible
	// and separately renamable/deletable (this is what stops sessions hiding).
	for _, rec := range registered {
		s := rec.Snapshot() // one consistent row per record, not a torn live read
		regDirs[s.Dir] = true
		// Newest activity across every id this session has run under (a clear/
		// compress rotates the live id but the old transcript keeps the newest
		// mtime until the next turn lands).
		var last int64
		for _, id := range s.TranscriptIDs() {
			if t := lastByID[id]; t > last {
				last = t
			}
		}
		if last == 0 {
			last = lastByDir[s.Dir]
		}
		views = append(views, discoveredView{
			Name: s.Name, Dir: s.Dir, SessionID: s.SessionID,
			LastActive: last, Active: active[s.Dir], Registered: true,
			Busy: c.srv.isBusy(s.SessionID), Target: sandboxTarget(s), Host: s.Host,
			Agent: s.Agent, Model: s.Model, Profile: s.Profile,
		})
	}
	// Unregistered sessions found on disk — one adoptable row per directory. The
	// walk reports every transcript, newest first; these aren't managed yet, so a
	// single per-dir entry (its most recent session — the one `claude --resume`
	// would continue) is enough to offer adoption.
	seenDir := map[string]bool{}
	for _, d := range found {
		if regDirs[d.Dir] || seenDir[d.Dir] {
			continue
		}
		seenDir[d.Dir] = true
		views = append(views, discoveredView{
			Name: sanitizeName(filepath.Base(d.Dir)), Dir: d.Dir, SessionID: d.SessionID,
			LastActive: d.LastActive, Active: active[d.Dir], Registered: false,
			// Discovery scans this machine's disk, so an unregistered find is local.
			Host: session.LocalHost,
		})
	}
	c.send(msgDiscovered(views, !ok))
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
	c.doRename(recName(rec), newName)
	c.doDiscover()
}

// doSetAgent switches a session's AI backend (and model) durably. It mirrors
// doRenameDiscovered: it locates the session by session_id (registering a still-
// discovered one by dir first), then stamps the resolved backend + a model valid
// for it (an explicit alias that isn't in the new backend's catalogue falls back to
// its default). Changing the backend rotates to a fresh session_id and un-Starts
// the session: Claude and Codex transcripts use incompatible on-disk formats, so the
// new AI can't resume the old one's history directly. Rather than drop that context,
// it carries a portable recap of the outgoing backend's transcript forward as
// PendingSeed, so the new backend continues the same conversation from its first turn
// (the old transcript stays on disk too). That recap is read off this goroutine and
// lands through the record's seed promise, so the switch answers immediately and the
// next turn waits for it (AwaitSeed) only if it gets there first. A backend whose
// transcript isn't readable yields an empty recap and switches clean.

// doSetAgent switches a session's AI backend (and model) durably. It mirrors
// doRenameDiscovered: it locates the session by session_id (registering a still-
// discovered one by dir first), then stamps the resolved backend + a model valid
// for it (an explicit alias that isn't in the new backend's catalogue falls back to
// its default). Changing the backend rotates to a fresh session_id and un-Starts
// the session: Claude and Codex transcripts use incompatible on-disk formats, so the
// new AI can't resume the old one's history directly. Rather than drop that context,
// it carries a portable recap of the outgoing backend's transcript forward as
// PendingSeed, so the new backend continues the same conversation from its first turn
// (the old transcript stays on disk too). That recap is read off this goroutine and
// lands through the record's seed promise, so the switch answers immediately and the
// next turn waits for it (AwaitSeed) only if it gets there first. A backend whose
// transcript isn't readable yields an empty recap and switches clean.
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
	cur := rec.Snapshot() // consistent pre-switch view of the record
	attachedHere := c.attached != nil && recID(c.attached) == cur.SessionID
	ag := c.srv.driver.Registry().Resolve(agentID)
	model := c.srv.driver.ProviderSettings().DefaultModel(ag)
	if modelAlias != "" {
		if m, ok := ag.Model(modelAlias); ok {
			model = m.Alias
		}
	}
	// Compare resolved ids so an empty/omitted agent (== the default backend) is a
	// no-op against a session already on that backend and doesn't force a rotation.
	curID := c.srv.driver.Registry().Resolve(cur.Agent).ID
	var oldID string
	if curID != ag.ID {
		if c.srv.isBusy(cur.SessionID) {
			c.fail("busy", "still working — switch the agent when the turn finishes")
			return
		}
		// Archive the outgoing backend as a display segment so its messages stay in
		// the chat log after the rotation drops them from context (rec.Agent/host/ids
		// still point at the old backend here). Skip an un-run backend — nothing to
		// show — so repeated no-op switches don't pile up empty segments.
		var seg *session.HistorySegment
		if cur.Started {
			s := c.srv.driver.ArchiveSegment(rec) // reads the record — compute it outside the lock
			seg = &s
		}
		newID, err := session.NewSessionID()
		if err != nil {
			c.fail("internal", err.Error())
			return
		}
		oldID = cur.SessionID
		// Carry the outgoing backend's conversation across the switch: read its
		// transcript through the generic per-backend reader and stash a portable recap
		// as the seed for the first turn on the new backend. Reading a whole chain is
		// an SSH round-trip per transcript, so it does NOT run here on the connection's
		// serial read loop — the rotation is stamped and answered immediately and the
		// recap lands through the record's seed promise (opened below, inside the same
		// critical section as the rotation, so a turn can never slip in between and see
		// a fresh context with no promise on it). A backend with no readable transcript
		// (e.g. antigravity's null reader) settles the promise with an empty recap, so
		// the switch is clean exactly as before.
		// One critical section for the whole rotation: a concurrent reader (another
		// device's attach, the job reconciler) must never see the new session_id with
		// the old backend's chain still attached.
		settleSeed := rec.MutateWithSeed(func(rec *session.Session) {
			if seg != nil {
				rec.History = append(rec.History, *seg)
			}
			rec.SessionID = newID
			rec.Started = false
			rec.AskPrimed = false
			rec.JobsPrimed = false        // re-prime the background-job instruction on the new backend
			rec.PriorIDs = nil            // don't chain the old backend's transcripts into the new one
			rec.PendingSeed = "" // ...a recap of them lands via settleSeed below instead
		})
		go c.srv.computeHandoffSeed(rec, cur, settleSeed)
	}
	rec.Mutate(func(rec *session.Session) {
		rec.Agent = ag.ID
		rec.Model = model
	})
	if err := c.srv.store.Put(rec); err != nil {
		c.fail("internal", err.Error())
		return
	}
	if oldID != "" {
		// The session_id rotated: move the hub + id index onto the new id so an
		// attached device still receives the next turn, and forget the old id.
		c.srv.rekeyJob(oldID, recID(rec))
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

// handoffSeedTimeout bounds the off-loop transcript read that builds a set_agent
// recap. It is generous — a long chain over SSH is several round-trips — because
// the only thing waiting on it is the session's next turn, and the alternative to
// waiting is starting the new backend with no memory of the conversation.
const handoffSeedTimeout = 2 * time.Minute

// computeHandoffSeed reads the OUTGOING backend's transcript chain and settles the
// rotated record's seed promise with a portable recap of it. It runs in its own
// goroutine off the connection's read loop (the read is one SSH round-trip per
// transcript) and on a background context, so the app's badge refresh doesn't wait
// on it and a device disconnecting mid-read doesn't cost the session its context.
// pre is the pre-rotation snapshot: the old backend, host and chain to read.
func (s *Server) computeHandoffSeed(rec *session.Session, pre *session.Session, settle func(string)) {
	ctx, cancel := context.WithTimeout(context.Background(), handoffSeedTimeout)
	defer cancel()
	var seed string
	if msgs, err := s.driver.ReadTranscriptChain(ctx, pre.Agent, pre.Host, pre.TranscriptIDs()); err != nil {
		log.Printf("set_agent[%s]: read prior transcript for handoff: %v", pre.Name, err)
	} else {
		seed = formatHandoffRecap(msgs)
	}
	settle(seed) // settles even on error/empty, so a waiting turn is never stranded
	if seed == "" {
		return
	}
	if err := s.store.Put(rec); err != nil { // persist the late seed across a restart
		log.Printf("set_agent[%s]: persist handoff seed: %v", pre.Name, err)
	}
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
		c.doAttach(recName(s), false)
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
	c.doAttach(recName(rec), false)
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
		dir = rec.Snapshot().Dir
	} else if p := c.srv.driver.TranscriptPathByID(c.ctx, "", sessionID); p != "" {
		dir = c.srv.driver.TranscriptCwd(c.ctx, "", p)
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
	if rec != nil {
		// Drop the record and push the refreshed lists FIRST, then purge in the
		// background: the row exists because the registry says so, and the disk
		// wipe (a backend-aware purge across every backend the session ran) plus
		// the container remove are best-effort work nothing here waits on.
		if c.attached != nil && recID(c.attached) == recID(rec) {
			c.doDetach()
		}
		name := recName(rec)
		if derr := c.srv.store.Delete(name); derr != nil {
			log.Printf("delete session record %s: %v", name, derr)
		}
		c.srv.dropJob(recID(rec))
		c.purgeAsync(rec, name, c.doDiscover)
	} else {
		// Unregistered rows come from the discovery scan on the loopback host
		// (TranscriptPathByID above reads the same place), so delete there. There's
		// no record to drop, so this one is what makes the row go away — it has to
		// finish before the refreshed discovery is pushed.
		if _, err := c.srv.driver.DeleteSessionsForDir(c.ctx, "", sessionID, dir); err != nil {
			c.fail("internal", err.Error())
			return
		}
	}
	c.sendSessionList()
	if rec == nil {
		c.doDiscover()
	}
}

// serveHistory returns a page of a session's past conversation, read from
// Claude's transcript on disk. `before` is the exclusive index cursor (nil =
// most recent page); the app pages older by passing the oldest index it holds.

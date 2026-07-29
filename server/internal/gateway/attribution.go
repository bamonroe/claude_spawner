package gateway

// Attribution — the single vocabulary for the metadata that must ride with every
// server→client frame so a client files it correctly: which SESSION a frame
// belongs to (`session_id`) and which TURN (`turn`). A client viewing one session
// keys an incoming frame by its `session_id`; a session-scoped frame that arrives
// without one is misfiled under whatever the client happens to be showing (and
// then "hops" as sessions switch).
//
// This used to be written three unrelated ways — builders that took a session id
// (`transcript`, `attached`, `history`, …), the hub sink stamping from the job,
// `stampTurn` wrapping terminals, plus a bespoke say/error sniff on the write path
// — so a frame sent down a path nobody had taught to stamp went out unattributed.
// The bug was the inevitable gap between those conventions.
//
// Now there is ONE resolver, [attribute], that every send path funnels through:
//   - the hub fan-out ([conn.jobSink]) attributes each frame to the job's session,
//     authoritatively — its sid tracks a compress rotation;
//   - a direct unicast ([conn.stampSession] at the write choke point) attributes a
//     session-scoped notice to the connection's current session, filling only when
//     the builder didn't already know it.
//
// Delivery stays two backends — plain unicast vs the hub's fan-out + per-connection
// buffering + mid-turn replay — because those are genuinely different transports;
// but both now speak this one attribution vocabulary instead of re-deriving it.

// attribute writes a frame's session/turn metadata onto the wire — the single
// point that sets `session_id`/`turn`. An empty session or turn leaves that field
// untouched. `overwrite` governs a `session_id` a builder already set: the hub is
// authoritative and replaces it (a compress rotation makes the job's sid the truth),
// while a direct notice fills only when absent, so a builder that knew its own
// session wins.
func attribute(m map[string]any, session, turn string, overwrite bool) {
	if session != "" {
		if cur, _ := m["session_id"].(string); overwrite || cur == "" {
			m["session_id"] = session
		}
	}
	if turn != "" {
		m["turn"] = turn
	}
}

// sessionScoped names the frame types that belong to a specific session's chat log
// and are sent DIRECTLY — not built with a session id, not fanned out by the hub —
// so their session must be filled from the connection at the write choke point.
// The choke point consults this registry instead of a hardcoded inline check, so
// declaring a new session-scoped direct type is a one-line edit that can't be
// forgotten (the failsafe: a registered type physically cannot leave unattributed
// while the connection is attached). Types whose builder already carries
// `session_id` (`transcript`, `attached`, `context_reset`, `renamed`, `history`)
// attribute at construction and are intentionally absent here; hub frames
// attribute via [conn.jobSink].
var sessionScoped = map[string]bool{
	"say":   true,
	"error": true,
}

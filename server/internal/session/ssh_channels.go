package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// One SSH connection is not an unlimited pipe. OpenSSH caps concurrent channels
// per connection at MaxSessions (default 10), and everything the server does on
// a host — every turn's stream, every file probe, every transcript read — shares
// the ONE pooled client for that host. The digest sweep alone fans out several
// reads at once; a concurrent attach's stat/tail plus a running turn's stream sit
// on the same connection, and past that ceiling the server refuses to open the
// channel.
//
// That refusal used to be indistinguishable from a dead link: the caller saw an
// error, assumed the cached connection was stale, and dropped + re-dialed the
// whole client — tearing down every OTHER in-flight operation on it, including a
// running turn, to "fix" a connection that was perfectly healthy. Momentary
// overload cascaded into a reconnect storm.
//
// So channel opening is budgeted HERE, in the pool that owns the connection,
// rather than left to each caller to be careful about:
//
//   - sshMaxChannels bounds in-flight channels per pooled client, below the
//     server's ceiling. Callers past the budget WAIT for a slot instead of
//     failing, which is the honest outcome — the work is queued, not lost.
//   - a channel-open refusal is returned as errChannelOpen, which the re-dial
//     paths treat as "the connection is fine, we're just busy" and never drop on.
//
// The two kinds of channel have very different lifetimes and must not compete
// on equal terms: a TURN's channel is held for the whole turn (minutes), while a
// probe is one round trip. Left in a single pool, a handful of concurrent turns
// on one host would hold every slot and starve every probe behind them — history
// and the digest sweep would just hang. So long-lived streams draw from a
// sub-budget of at most sshMaxStreamChannels, guaranteeing the remainder stays
// available to short operations no matter how many turns are running.
//
// Every channel on a pooled client goes through openChannel, so the budget is a
// property of the pool and not a rule five call sites have to remember.
//
// The budget bounds ONE connection, and a connection is no longer the unit of a
// host's concurrency: when every cached connection to a host is at its budget, the
// pool dials another rather than making the caller queue (see poolEntry). So
// sshMaxChannels is a per-link ceiling and sshMaxConnsPerHost × sshMaxChannels is
// the host's real one — only there does a caller block.
//
// Those extra connections are opened for a BURST — a digest sweep, a prefetch
// storm — and the burst ends. sshIdleConnTTL bounds how long one lingers with
// nothing on it: past that it is retired and closed (see reapIdle). The host's
// FIRST connection is never reaped; it is the cached one the keepalive owns, and
// dropping it would just make the next caller pay a dial.
const (
	sshMaxChannels       = 8
	sshMaxStreamChannels = 4
	sshMaxConnsPerHost   = 4
	sshIdleConnTTL       = 2 * time.Minute
)

// probeChannelCeilingMax bounds the diagnostic below so a peer with a very
// generous (or absent) MaxSessions can't have us open channels forever.
const probeChannelCeilingMax = 64

// ProbeChannelCeiling measures how many concurrent session channels a host will
// actually grant on ONE connection, so sshMaxChannels can be set from a measured
// number rather than from OpenSSH's documented default. It opens channels on the
// pooled client one at a time until the peer refuses, then closes them all and
// returns the count reached (plus the refusal that ended it, if any).
//
// It deliberately bypasses the channel budget — the budget is the thing being
// calibrated. That also makes it a diagnostic and not something to run against a
// busy host: while it runs it holds most of that connection's channels, so real
// work queues behind it. Call it from a gated live test or a one-off check.
func (p *SSHPool) ProbeChannelCeiling(ctx context.Context, host string) (int, error) {
	client, err := p.client(host)
	if err != nil {
		return 0, err
	}
	var open []*ssh.Session
	defer func() {
		for _, s := range open {
			s.Close()
		}
	}()
	for len(open) < probeChannelCeilingMax {
		if err := ctx.Err(); err != nil {
			return len(open), err
		}
		sess, err := client.NewSession()
		if err != nil {
			return len(open), fmt.Errorf("%w: %v", errChannelOpen, err)
		}
		open = append(open, sess)
	}
	return len(open), nil
}

// errChannelOpen marks a failure to OPEN a channel on an otherwise-live
// connection (the peer's MaxSessions ceiling). Distinct from a transport error:
// re-dialing cannot help, and dropping the client would punish every other
// operation sharing it.
var errChannelOpen = errors.New("ssh: could not open channel")

// isChannelOpenErr reports whether err came from channel opening rather than the
// transport, so a caller knows not to evict the pooled client over it.
func isChannelOpenErr(err error) bool { return errors.Is(err, errChannelOpen) }

// isRemoteExitErr reports whether err is the REMOTE COMMAND's non-zero exit
// status. That is the connection working perfectly: the channel opened, the
// command ran, and it chose to fail. `grep` finding nothing, `[ -e ]` on a glob
// that matched nothing, `rm` on an absent path — all of these are exit 1 on a
// healthy link.
func isRemoteExitErr(err error) bool {
	var exit *ssh.ExitError
	return errors.As(err, &exit)
}

// shouldRedial is the pool's single answer to "did this error mean the CONNECTION
// is broken?" — the question every operation on a pooled client has to get right,
// because the remedy (drop + re-dial) tears down every OTHER channel on that
// connection, a running turn's stream included.
//
// Only a transport error qualifies. The two impostors are a channel-open refusal
// (the peer's MaxSessions ceiling: busy, not dead) and a remote command's exit
// status (the command failed, not the link). Treating an exit status as a dead
// connection is not a theoretical worry: the deferred-purge retry ran a command
// that exits 1 whenever the files it deletes are already gone, on a six-minute
// ticker, and each pass dropped the shared connection out from under whatever turn
// was streaming on it — the user saw "stream ended without a result event" every
// six minutes, with nothing in the log connecting the two.
//
// It lives here, next to the budget, so the policy is a property of the pool
// rather than a condition each of Run/WriteFile/Stream has to restate correctly.
func shouldRedial(err error) bool {
	return err != nil && !isChannelOpenErr(err) && !isRemoteExitErr(err)
}

// channelBudget is one pooled connection's channel allowance: a counting
// semaphore with a slot per permitted concurrent channel.
type channelBudget struct {
	once    sync.Once
	slot    chan struct{} // every channel, whatever its lifetime
	streams chan struct{} // the long-lived subset (turns), so they can't take every slot
}

func (b *channelBudget) init() {
	b.once.Do(func() {
		b.slot = make(chan struct{}, sshMaxChannels)
		b.streams = make(chan struct{}, sshMaxStreamChannels)
	})
}

// acquire blocks for a channel slot (or until ctx ends). A long-lived caller
// must also hold a stream slot, taken FIRST so a queued turn waiting for its
// sub-budget isn't sitting on a general slot a probe could be using.
func (b *channelBudget) acquire(ctx context.Context, longLived bool) error {
	b.init()
	if longLived {
		select {
		case b.streams <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	select {
	case b.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		if longLived {
			<-b.streams
		}
		return ctx.Err()
	}
}

// tryAcquire takes a slot only if one is free right now, never blocking. It is how
// a caller is offered around the host's connections: the first one with room wins,
// and "nobody has room" is the signal to dial another connection rather than wait.
func (b *channelBudget) tryAcquire(longLived bool) bool {
	b.init()
	if longLived {
		select {
		case b.streams <- struct{}{}:
		default:
			return false
		}
	}
	select {
	case b.slot <- struct{}{}:
		return true
	default:
		if longLived {
			<-b.streams
		}
		return false
	}
}

// load is the number of channels currently charged to this connection, used to
// hand a caller the least busy one.
func (b *channelBudget) load() int {
	b.init()
	return len(b.slot)
}

func (b *channelBudget) release(longLived bool) {
	b.init()
	select {
	case <-b.slot:
	default: // never block; a double release would be a bug, not a hang
	}
	if longLived {
		select {
		case <-b.streams:
		default:
		}
	}
}

// leastLoaded returns the live connection carrying the fewest channels, or nil if
// there are none. Spreading channels evenly is what keeps a single long turn from
// pinning one link while its siblings sit idle.
func leastLoaded(conns []*pooledConn) *pooledConn {
	var best *pooledConn
	for _, c := range conns {
		if !c.live() {
			continue
		}
		if best == nil || c.chans.load() < best.chans.load() {
			best = c
		}
	}
	return best
}

// acquireConn hands back a connection to host with a channel slot already taken on
// it, plus the release for that slot. The order is: use the least-loaded existing
// connection that has room; if none does, dial an ADDITIONAL one (up to
// sshMaxConnsPerHost) so the caller isn't punished for a busy link; only once the
// host is at its connection cap does the caller block on a slot.
func (p *SSHPool) acquireConn(ctx context.Context, host string, longLived bool) (*pooledConn, func(), error) {
	e := p.entry(host)
	for {
		// Lazily retire extras the last burst left behind, so a host that went quiet
		// converges back to one connection without a background goroutine.
		p.reapIdle(host)

		e.mu.Lock()
		conns := slices.Clone(e.conns)
		e.mu.Unlock()

		// Cheapest first: somebody already connected has room.
		for _, c := range byLoad(conns) {
			if c.chans.tryAcquire(longLived) {
				// It may have been retired between the load check and the slot: hand
				// back a connection that is still eligible, or look again.
				if !c.live() {
					c.chans.release(longLived)
					continue
				}
				return c, releaser(c, longLived), nil
			}
		}

		// Everything is saturated (or there is nothing yet). Add a connection —
		// under the entry lock, so concurrent callers for this host don't each dial
		// one and blow past the cap. A caller that arrives to find the slice already
		// grown just loops and re-tries the fast path.
		e.mu.Lock()
		grew := len(e.conns) != len(conns)
		atCap := len(e.conns) >= sshMaxConnsPerHost
		var c *pooledConn
		var err error
		if !grew && !atCap {
			c, err = p.dialLocked(host, e)
		}
		e.mu.Unlock()
		if err != nil {
			// The extra dial failed. If the host has no connection at all that is
			// the caller's error; but when live connections exist they are merely
			// BUSY, and failing the caller because a bonus dial didn't land would be
			// a regression on the old single-connection behaviour, which waited —
			// so fall through to the wait below.
			if len(byLoad(conns)) == 0 {
				return nil, nil, err
			}
		}
		if c != nil {
			if c.chans.tryAcquire(longLived) {
				return c, releaser(c, longLived), nil
			}
			continue // raced with other callers onto the new connection; look again
		}
		if grew {
			continue
		}

		// At the cap: this is the point where waiting is the honest answer. Queue on
		// the least-loaded link, then make sure it wasn't evicted while we waited.
		target := leastLoaded(conns)
		if target == nil {
			continue
		}
		if err := target.chans.acquire(ctx, longLived); err != nil {
			return nil, nil, err
		}
		if !target.live() {
			target.chans.release(longLived)
			continue
		}
		return target, releaser(target, longLived), nil
	}
}

// byLoad orders live connections least-busy first.
func byLoad(conns []*pooledConn) []*pooledConn {
	live := make([]*pooledConn, 0, len(conns))
	for _, c := range conns {
		if c.live() {
			live = append(live, c)
		}
	}
	slices.SortFunc(live, func(a, b *pooledConn) int { return a.chans.load() - b.chans.load() })
	return live
}

// releaser returns the once-only function that gives a slot back to its connection.
func releaser(c *pooledConn, longLived bool) func() {
	var once sync.Once
	return func() { once.Do(func() { c.chans.release(longLived) }) }
}

// openChannel opens one SSH session channel on a pooled connection to host, having
// first taken a slot in that connection's channel budget — dialing another
// connection to the host if every existing one is full. The connection it chose is
// returned so a caller whose operation fails with a TRANSPORT error can drop
// exactly that one (see shouldRedial); the release MUST be called exactly once when
// the channel is done — defer it next to the session's Close, or hand it to
// whatever owns the session's lifetime (see sshProc).
//
// longLived marks a channel held for the duration of a turn rather than a single
// round trip; those draw from the stream sub-budget so they can't starve probes.
//
// A refusal from the peer comes back wrapped in errChannelOpen so the caller's
// re-dial path can tell "connection busy" from "connection dead".
func (p *SSHPool) openChannel(ctx context.Context, host string, longLived bool) (*ssh.Session, *pooledConn, func(), error) {
	client, release, err := p.acquireConn(ctx, host, longLived)
	if err != nil {
		return nil, nil, nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		release()
		return nil, client, nil, fmt.Errorf("%w: %v", errChannelOpen, err)
	}
	// Count the channel against the connection so an eviction can defer its close
	// until this operation is done — see pooledConn.
	client.hold()
	var once sync.Once
	return sess, client, func() { once.Do(func() { client.done(); release() }) }, nil
}

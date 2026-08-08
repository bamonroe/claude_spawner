package session

import (
	"context"
	"errors"
	"fmt"
	"sync"

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
const (
	sshMaxChannels       = 8
	sshMaxStreamChannels = 4
)

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

// openChannel opens one SSH session channel on host's pooled client, waiting for
// a slot in that connection's channel budget first. The returned release MUST be
// called exactly once when the channel is done — defer it next to the session's
// Close, or hand it to whatever owns the session's lifetime (see sshProc).
//
// longLived marks a channel held for the duration of a turn rather than a single
// round trip; those draw from the stream sub-budget so they can't starve probes.
//
// A refusal from the peer comes back wrapped in errChannelOpen so the caller's
// re-dial path can tell "connection busy" from "connection dead".
func (p *SSHPool) openChannel(ctx context.Context, host string, client *ssh.Client, longLived bool) (*ssh.Session, func(), error) {
	b := p.entry(host).budget()
	if err := b.acquire(ctx, longLived); err != nil {
		return nil, nil, err
	}
	sess, err := client.NewSession()
	if err != nil {
		b.release(longLived)
		return nil, nil, fmt.Errorf("%w: %v", errChannelOpen, err)
	}
	var once sync.Once
	return sess, func() { once.Do(func() { b.release(longLived) }) }, nil
}

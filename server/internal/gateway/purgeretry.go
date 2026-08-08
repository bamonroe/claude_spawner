package gateway

import (
	"context"
	"time"
)

// purgeRetryTick is how often the server retries transcript purges owed by
// deletes made while a host was unreachable. Deliberately slow: the debt is
// durable, nobody is waiting on it, and each pass costs only a non-blocking
// reachability check per owed item while its host stays down. It's a little
// longer than the SSH pool's maximum negative-dial backoff, so a host that has
// come back is generally dialable again by the time we look.
const purgeRetryTick = 6 * time.Minute

// purgeRetryLoop drains session.PurgeQueue in the background: a session deleted
// while its host was offline drops out of the registry immediately, and its
// remote transcripts are swept up here once the host answers again. Started once
// from New().
func (s *Server) purgeRetryLoop() {
	t := time.NewTicker(purgeRetryTick)
	defer t.Stop()
	for range t.C {
		s.driver.RetryPurges(context.Background())
	}
}

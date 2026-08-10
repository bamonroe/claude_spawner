package gateway

import (
	"context"
	"strings"
	"sync"

	"github.com/bam/claude_spawner/server/internal/session"
)

// The re-login flow over the wire. The host's `claude` can lose its credentials at
// any time, and until now fixing that meant a shell on the host — so the whole
// point of these five messages is that the phone can do it: ask who a host is
// logged in as (`auth_status`), start a login (`auth_login`), hand back the OAuth
// URL the CLI prints (`auth_login_url`), feed the pasted code in
// (`auth_login_code`), and report the verdict (`auth_login_result`).
//
// The driver underneath is `claude auth`, not the interactive /login TUI — see
// internal/session/authlogin.go and docs/architecture.md. That keeps the
// no-TUI-scraping rule intact: we read a URL out of a PTY and write one code back,
// nothing more.
//
// A login is per-HOST, not per-connection: it authenticates a $HOME on a machine,
// so a second phone asking about the same host is asking about the same login, and
// the answers broadcast to every client rather than only the one that asked.

// Only ONE login may be live per host at a time, and that is an invariant of this
// registry rather than something each caller remembers. Every `claude auth login`
// mints its own code_challenge/state, so a second process silently invalidates the
// first one's URL: two racing logins means the user pastes a good code into a dead
// challenge. So a start either *joins* the pending login (and gets its
// already-issued URL) or explicitly cancels it and takes its place — the slot is
// claimed before the process is spawned, so two simultaneous requests can't both
// end up with a PTY.
//
// pendingLogin is that slot: the claim, the process once it exists, and the URL
// once the CLI prints it, so a joiner can wait on the same result the starter does.
type pendingLogin struct {
	host  string
	want  session.AuthLoginMode // identity the starter asked for ("" = whatever the host has)
	ready chan struct{}         // closed once mode/url/err are set
	mode  session.AuthLoginMode // the identity actually used; read only after ready
	url   string
	err   error

	login    *session.AuthLogin // nil until StartAuthLogin returns
	watchMu  sync.Mutex
	watchers map[*conn]bool // connections that care; empty = nobody left to finish it
}

// waitURL blocks until the login's URL is known, the attempt fails, or ctx ends.
func (p *pendingLogin) waitURL(ctx context.Context) (string, error) {
	select {
	case <-p.ready:
		return p.url, p.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// resolve publishes the outcome of the start exactly once.
func (p *pendingLogin) resolve(l *session.AuthLogin, mode session.AuthLoginMode, url string, err error) {
	p.login, p.mode, p.url, p.err = l, mode, url, err
	close(p.ready)
}

// cancel kills the underlying process if one was ever started.
func (p *pendingLogin) cancel() {
	if p.login != nil {
		p.login.Close()
	}
}

func (p *pendingLogin) watch(c *conn) {
	p.watchMu.Lock()
	defer p.watchMu.Unlock()
	p.watchers[c] = true
}

// unwatch drops c and reports whether anyone is still waiting on this login.
func (p *pendingLogin) unwatch(c *conn) bool {
	p.watchMu.Lock()
	defer p.watchMu.Unlock()
	delete(p.watchers, c)
	return len(p.watchers) > 0
}

// claimLogin is the single-flight gate. It returns (mine, nil) when the caller now
// owns the host's login slot and must start the process, or (nil, existing) when a
// login is already pending and the caller should ride along with it. A request that
// names a *different* identity than the pending one cancels it and claims the slot,
// since the two can't both be what the user meant.
func (s *Server) claimLogin(c *conn, host string, mode session.AuthLoginMode) (mine, existing *pendingLogin) {
	s.authMu.Lock()
	if cur := s.logins[host]; cur != nil {
		if mode == "" || mode == cur.want {
			s.authMu.Unlock()
			cur.watch(c)
			return nil, cur
		}
		delete(s.logins, host)
		s.authMu.Unlock()
		go cur.cancel()
		s.authMu.Lock()
	}
	p := &pendingLogin{host: host, want: mode, ready: make(chan struct{}), watchers: map[*conn]bool{}}
	s.logins[host] = p
	s.authMu.Unlock()
	p.watch(c)
	return p, nil
}

// loginFor returns the in-flight login on host, if any.
func (s *Server) loginFor(host string) *pendingLogin {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.logins[host]
}

// dropLogin forgets a finished login (only if it is still the current one, so a
// superseded attempt's cleanup can't evict its replacement).
func (s *Server) dropLogin(host string, p *pendingLogin) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.logins[host] == p {
		delete(s.logins, host)
	}
}

// releaseLogins is called when a connection goes away. A login only exists to be
// finished by a client pasting a code, so once the last client watching one is gone
// (the app was killed mid-flow) the PTY is stranded — kill it rather than leave it
// holding an SSH channel until the driver's timeout. Another still-connected client
// watching the same host keeps it alive.
func (s *Server) releaseLogins(c *conn) {
	s.authMu.Lock()
	pending := make([]*pendingLogin, 0, len(s.logins))
	for _, p := range s.logins {
		pending = append(pending, p)
	}
	s.authMu.Unlock()
	for _, p := range pending {
		if !p.unwatch(c) {
			s.dropLogin(p.host, p)
			go p.cancel()
		}
	}
}

// authHost resolves the host an auth message targets: the named one, else the
// server's configured target host.
func (c *conn) authHost(name string) string {
	if h := strings.TrimSpace(name); h != "" {
		return h
	}
	return c.srv.targetHost()
}

// doAuthStatus answers "is this host's claude logged in, and as whom" — the
// pre-check the client shows before offering a login and the post-check that
// confirms one took. Logged out is a successful answer, not an error.
func (c *conn) doAuthStatus(hostName string) {
	host := c.authHost(hostName)
	if c.srv.ssh == nil {
		c.fail("auth_failed", "ssh is disabled, so I can't check the login")
		return
	}
	go func() {
		st, err := session.CheckAuthStatus(c.ctx, c.srv.ssh, host, "")
		if err != nil {
			c.fail("auth_failed", err.Error())
			return
		}
		c.srv.broadcast(msgAuthStatus(host, st))
	}()
}

// doAuthLogin starts `claude auth login` on a host and streams the flow back: the
// OAuth URL as soon as the CLI prints it, then the verdict once the process exits.
// method picks the billing identity ("claudeai" | "console"); empty reuses whatever
// the host is already on, so a plain re-login never makes the user re-pick.
func (c *conn) doAuthLogin(hostName, method string) {
	host := c.authHost(hostName)
	if c.srv.ssh == nil {
		c.fail("auth_failed", "ssh is disabled, so I can't log in")
		return
	}
	go func() {
		requested := session.AuthLoginMode(strings.TrimSpace(method))
		p, existing := c.srv.claimLogin(c, host, requested)
		if existing != nil {
			// Someone is already logging this host in. Re-send that attempt's URL to
			// the asker instead of minting a second challenge that would kill it.
			url, err := existing.waitURL(c.ctx)
			if err != nil {
				return // the starting goroutine reports the failure
			}
			c.send(msgAuthLoginURL(host, string(existing.mode), url))
			return
		}
		defer c.srv.dropLogin(host, p)

		mode := requested
		if mode == "" {
			// Match the identity the host already had; a logged-out host defaults to
			// the subscription flow, which is the CLI's own default.
			if st, err := session.CheckAuthStatus(c.ctx, c.srv.ssh, host, ""); err == nil {
				mode = st.Mode()
			} else {
				mode = session.AuthLoginClaudeAI
			}
		}
		l, err := session.StartAuthLogin(context.Background(), c.srv.ssh, host, "", mode, 0)
		if err != nil {
			p.resolve(nil, mode, "", err)
			c.fail("auth_failed", err.Error())
			return
		}

		url, err := l.URL(c.ctx)
		if err != nil {
			p.resolve(l, mode, "", err)
			c.srv.broadcast(msgAuthLoginResult(host, false, err.Error()))
			l.Close()
			return
		}
		p.resolve(l, mode, url, nil)
		c.srv.broadcast(msgAuthLoginURL(host, string(mode), url))

		// Wait spans the human's whole browser detour; the driver's own timeout is
		// what stops an abandoned attempt from pinning the channel forever.
		if err := l.Wait(); err != nil {
			c.srv.broadcast(msgAuthLoginResult(host, false, err.Error()))
			return
		}
		c.srv.broadcast(msgAuthLoginResult(host, true, ""))
		// The exit code says it worked; the status says as whom — so re-check and
		// push the fresh identity out without the client having to ask.
		if st, err := session.CheckAuthStatus(c.ctx, c.srv.ssh, host, ""); err == nil {
			c.srv.broadcast(msgAuthStatus(host, st))
		}
	}()
}

// doAuthLoginCode hands the code the user pasted out of the browser to the waiting
// login process. The verdict comes back later as `auth_login_result` from the
// goroutine that started the login, not from here.
func (c *conn) doAuthLoginCode(hostName, code string) {
	host := c.authHost(hostName)
	p := c.srv.loginFor(host)
	if p == nil {
		c.fail("auth_failed", "there's no login waiting on "+host)
		return
	}
	// The code can only exist because the URL was handed out, so the slot is
	// resolved by now; check anyway rather than write into a nil process.
	select {
	case <-p.ready:
	default:
		c.fail("auth_failed", "the login on "+host+" hasn't started yet")
		return
	}
	if p.login == nil {
		c.fail("auth_failed", "the login on "+host+" never started")
		return
	}
	if err := p.login.SubmitCode(code); err != nil {
		c.fail("auth_failed", err.Error())
	}
}

// doAuthLoginCancel abandons an in-flight login (the user navigated away). The
// login goroutine reports the resulting failure as `auth_login_result`.
func (c *conn) doAuthLoginCancel(hostName string) {
	host := c.authHost(hostName)
	p := c.srv.loginFor(host)
	if p == nil {
		return
	}
	c.srv.dropLogin(host, p)
	go p.cancel()
}

// doAuthLogout drops the host's credentials, then re-reports status so every client
// sees the logged-out state rather than a stale identity.
func (c *conn) doAuthLogout(hostName string) {
	host := c.authHost(hostName)
	if c.srv.ssh == nil {
		c.fail("auth_failed", "ssh is disabled, so I can't log out")
		return
	}
	go func() {
		if err := session.Logout(c.ctx, c.srv.ssh, host, ""); err != nil {
			c.fail("auth_failed", err.Error())
			return
		}
		st, err := session.CheckAuthStatus(c.ctx, c.srv.ssh, host, "")
		if err != nil {
			c.fail("auth_failed", err.Error())
			return
		}
		c.srv.broadcast(msgAuthStatus(host, st))
	}()
}

// The voice entry points. They are thin wrappers over the message handlers above
// because the flow is identical — the only difference is that a spoken command has
// no UI to read the answer off, so it also has to *say* something. The sign-in URL
// itself is never spoken: it's unusable as speech, so the voice reply points at the
// panel the `auth_login_url` message already opens in the app.
func (c *conn) doAuthStatusVoice() {
	host := c.authHost("")
	if c.srv.ssh == nil {
		c.send(msgSay("ssh is disabled, so I can't check the login."))
		return
	}
	go func() {
		st, err := session.CheckAuthStatus(c.ctx, c.srv.ssh, host, "")
		if err != nil {
			c.send(msgSay("couldn't check the login: " + err.Error()))
			return
		}
		c.srv.broadcast(msgAuthStatus(host, st))
		c.send(msgSay(host + " is " + st.Describe() + "."))
	}()
}

func (c *conn) doAuthLoginVoice() {
	if c.srv.ssh == nil {
		c.send(msgSay("ssh is disabled, so I can't log in."))
		return
	}
	c.send(msgSay("starting the claude login on " + c.authHost("") + ". check the app for the sign-in link."))
	c.doAuthLogin("", "")
}

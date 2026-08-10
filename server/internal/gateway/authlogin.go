package gateway

import (
	"context"
	"strings"

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

// loginFor returns the in-flight login on host, if any.
func (s *Server) loginFor(host string) *session.AuthLogin {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	return s.logins[host]
}

// putLogin records an in-flight login, superseding (and killing) any earlier one on
// the same host — the older URL is dead the moment a new process mints a challenge,
// so keeping it around would only let the user paste a code into a corpse.
func (s *Server) putLogin(host string, l *session.AuthLogin) {
	s.authMu.Lock()
	old := s.logins[host]
	s.logins[host] = l
	s.authMu.Unlock()
	if old != nil {
		go old.Close()
	}
}

// dropLogin forgets a finished login (only if it is still the current one, so a
// superseded attempt's cleanup can't evict its replacement).
func (s *Server) dropLogin(host string, l *session.AuthLogin) {
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.logins[host] == l {
		delete(s.logins, host)
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
		mode := session.AuthLoginMode(strings.TrimSpace(method))
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
			c.fail("auth_failed", err.Error())
			return
		}
		c.srv.putLogin(host, l)
		defer c.srv.dropLogin(host, l)

		url, err := l.URL(c.ctx)
		if err != nil {
			c.srv.broadcast(msgAuthLoginResult(host, false, err.Error()))
			l.Close()
			return
		}
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
	l := c.srv.loginFor(host)
	if l == nil {
		c.fail("auth_failed", "there's no login waiting on "+host)
		return
	}
	if err := l.SubmitCode(code); err != nil {
		c.fail("auth_failed", err.Error())
	}
}

// doAuthLoginCancel abandons an in-flight login (the user navigated away). The
// login goroutine reports the resulting failure as `auth_login_result`.
func (c *conn) doAuthLoginCancel(hostName string) {
	host := c.authHost(hostName)
	l := c.srv.loginFor(host)
	if l == nil {
		return
	}
	go l.Close()
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

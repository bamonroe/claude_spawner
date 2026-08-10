package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Re-authenticating a host's `claude` is a different shape of work from a turn, so
// it gets its own supervisor rather than riding the Executor path. A turn is
// fire-and-forget: build an argv, stream stream-json stdout, wait for exit. A login
// is long-lived and conversational — start the process, wait for it to print an
// OAuth URL, hand that URL to a human who completes the flow in whatever browser
// they like (the redirect_uri is a hosted page, so nothing has to run on this box),
// then feed exactly one pasted code back and wait for the exit code to say whether
// it took. See docs/scopes/claude-auth-login.md for the probe this is built from.
//
// The load-bearing constraint is the PTY. `claude auth login` reads its "Paste code
// here if prompted >" from the terminal, not from stdin: under a plain pipe the
// process ignores whatever is written and hangs forever. So unlike SSHExecutor —
// which deliberately requests no PTY to keep stream-json stdout clean — the login
// driver allocates one, and pays for it by having to strip ANSI/OSC escapes back
// out of the output before it can read the URL.

// AuthLoginMode selects which billing identity `claude auth login` authenticates
// against: the Claude.ai subscription flow (the CLI's own default) or the
// API/console flow. They mint URLs on different hosts, which is why URL extraction
// has to accept both.
type AuthLoginMode string

const (
	// AuthLoginClaudeAI is the subscription path (`--claudeai`).
	AuthLoginClaudeAI AuthLoginMode = "claudeai"
	// AuthLoginConsole is the API/console-billing path (`--console`).
	AuthLoginConsole AuthLoginMode = "console"
)

func (m AuthLoginMode) flag() (string, error) {
	switch m {
	case AuthLoginClaudeAI, "":
		return "--claudeai", nil
	case AuthLoginConsole:
		return "--console", nil
	default:
		return "", fmt.Errorf("unknown auth login mode %q", m)
	}
}

// DefaultAuthLoginTimeout bounds one login attempt end to end. The CLI itself waits
// at the paste prompt forever, so without a ceiling an abandoned login would pin an
// SSH channel and a remote process until the server restarted. Long enough that a
// human can finish the browser flow on a phone; short enough that a forgotten
// attempt cleans itself up.
const DefaultAuthLoginTimeout = 10 * time.Minute

// ErrAuthLoginExited reports that the login process died before it ever printed a
// URL — a broken binary, a missing host, a `claude` too old to have the subcommand.
// The wrapped error carries the exit status and Output() has the reason text.
var ErrAuthLoginExited = errors.New("auth login exited before emitting a URL")

// ErrAuthLoginCodeSubmitted guards the one-code rule: each invocation mints a fresh
// code_challenge bound to that process, so a second code is never meaningful.
var ErrAuthLoginCodeSubmitted = errors.New("auth login code already submitted")

// AuthLogin is one in-flight `claude auth login` on a host. Its lifecycle is
// strictly linear: Start → URL → SubmitCode → Wait, with Close usable at any point
// to abandon it. It is safe to call from multiple goroutines (the gateway reads the
// URL on one and submits the code on another), but it is single-use — a retry means
// a new AuthLogin, because restarting invalidates the previous URL.
type AuthLogin struct {
	host string
	mode AuthLoginMode

	sess    *ssh.Session
	stdin   io.WriteCloser
	release func()
	stop    func() bool
	cancel  context.CancelFunc

	out *stderrTail // bounded transcript of the PTY, ANSI included

	urlCh chan string // buffered(1); the first URL seen

	codeOnce sync.Once

	waitOnce sync.Once
	waitErr  error
	done     chan struct{} // closed when the process has exited
}

// StartAuthLogin launches `claude auth login` under a PTY on host and returns as
// soon as the process is running — before any URL exists, since the caller will
// usually want to surface "logging in…" immediately and fill the URL in when it
// arrives. bin overrides the host's registered claude binary; empty defers to the
// registry the same way a turn does.
//
// The process runs in, and as, whatever the host's login shell resolves — its $HOME
// is what decides which identity gets authenticated, which is exactly the knob the
// probe found. Choosing that identity is a matter of pointing at the right host
// entry, not of overriding HOME here.
//
// ctx bounds the whole attempt: cancelling it (or Close) kills the remote process.
// The returned AuthLogin must be Closed or Waited or it leaks an SSH channel.
func StartAuthLogin(ctx context.Context, pool *SSHPool, host, bin string, mode AuthLoginMode, timeout time.Duration) (*AuthLogin, error) {
	if pool == nil {
		return nil, errors.New("auth login: no ssh pool")
	}
	if host == "" {
		return nil, errors.New("auth login: no host")
	}
	flag, err := mode.flag()
	if err != nil {
		return nil, err
	}
	if bin == "" {
		bin = pool.binFor(host)
	}
	if timeout <= 0 {
		timeout = DefaultAuthLoginTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)

	// A login holds its channel for the whole human-paced flow, so it draws on the
	// long-lived sub-budget like a turn does and returns the slot only at exit.
	sess, _, release, err := pool.openChannel(ctx, host, true)
	if err != nil {
		cancel()
		return nil, err
	}
	fail := func(err error) (*AuthLogin, error) {
		_ = sess.Close()
		release()
		cancel()
		return nil, err
	}
	// Echo off so the pasted code doesn't come back at us interleaved with the
	// CLI's own output; "dumb" keeps the CLI from drawing a full-screen UI, though
	// it still emits OSC-8 hyperlinks around the URL, which stripEscapes handles.
	modes := ssh.TerminalModes{ssh.ECHO: 0, ssh.ICANON: 0}
	if err := sess.RequestPty("dumb", 40, 120, modes); err != nil {
		return fail(fmt.Errorf("auth login: request pty: %w", err))
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		return fail(fmt.Errorf("auth login: stdin: %w", err))
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		return fail(fmt.Errorf("auth login: stdout: %w", err))
	}
	// Under a PTY stderr is merged into the same stream by the terminal, so there is
	// only one channel of output to read — and no room for the pgid sentinel trick
	// SSHExecutor uses. Cancellation therefore closes the channel instead (see below).
	cmd := "cd \"$HOME\" && exec " + shellQuote(bin) + " auth login " + flag
	if err := sess.Start(cmd); err != nil {
		return fail(fmt.Errorf("auth login: start: %w", err))
	}

	l := &AuthLogin{
		host:    host,
		mode:    mode,
		sess:    sess,
		stdin:   stdin,
		release: release,
		cancel:  cancel,
		out:     &stderrTail{},
		urlCh:   make(chan string, 1),
		done:    make(chan struct{}),
	}
	// Abandoning a login has to actually stop the remote process, or the next
	// attempt races a stale one for the same identity. setsid is deliberately NOT
	// used here (it would detach the controlling terminal and break the prompt), so
	// the kill is a signal plus a channel close, which HUPs the process.
	l.stop = context.AfterFunc(ctx, func() {
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
	})
	go l.scan(stdout)
	return l, nil
}

// Host reports the host this login is running on.
func (l *AuthLogin) Host() string { return l.host }

// Mode reports which login flow is in progress.
func (l *AuthLogin) Mode() AuthLoginMode { return l.mode }

// scan reads the PTY until EOF, keeping a bounded transcript and publishing the
// first OAuth URL it sees. It reads raw bytes rather than lines because the prompt
// ("Paste code here if prompted >") is written without a trailing newline, so a
// line scanner would block on it — and because the URL arrives wrapped in escape
// bytes that have to be stripped before matching.
func (l *AuthLogin) scan(r io.Reader) {
	var pending strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			_, _ = l.out.Write(buf[:n])
			pending.Write(buf[:n])
			if u := findLoginURL(stripEscapes(pending.String())); u != "" {
				select {
				case l.urlCh <- u:
				default: // already published; the PTY prints it twice
				}
				pending.Reset()
			}
			// Don't let an un-matched prefix grow without bound while we wait at
			// the paste prompt; the URL always arrives in the first burst.
			if pending.Len() > 1<<16 {
				pending.Reset()
			}
		}
		if err != nil {
			return
		}
	}
}

// URL blocks until the login process prints its OAuth URL, and returns it. The URL
// is bound to this process: it stays valid only while this AuthLogin is alive, and
// starting another login invalidates it. If the process exits first, URL reports
// ErrAuthLoginExited with the transcript, since that is the only place the reason
// (an unknown subcommand, a missing binary) is written.
func (l *AuthLogin) URL(ctx context.Context) (string, error) {
	select {
	case u := <-l.urlCh:
		l.urlCh <- u // keep it readable again; the value is idempotent
		return u, nil
	case <-l.done:
		return "", fmt.Errorf("%w: %v: %s", ErrAuthLoginExited, l.waitErr, l.Output())
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// SubmitCode writes the code the user pasted from the browser to the PTY, exactly
// once. A carriage return terminates it because that is what a real terminal sends
// for Enter. After this the caller waits on Wait for the verdict — a bad code is
// not an error here, it is a nonzero exit there.
func (l *AuthLogin) SubmitCode(code string) error {
	err := ErrAuthLoginCodeSubmitted
	l.codeOnce.Do(func() {
		_, err = io.WriteString(l.stdin, strings.TrimSpace(code)+"\r")
		if err != nil {
			err = fmt.Errorf("auth login: submit code: %w", err)
		}
	})
	return err
}

// Wait blocks until the login process exits and reports the verdict: nil means the
// CLI authenticated successfully (exit 0), and any error carries the transcript
// tail, which is where the CLI writes its "Login failed: …" reason. Callers that
// want the resulting identity should follow a nil return with an auth-status check
// — the exit code says it worked, the status says as whom.
//
// Wait is idempotent and releases every resource the login held.
func (l *AuthLogin) Wait() error {
	l.waitOnce.Do(func() {
		if l.stop != nil {
			l.stop()
		}
		err := l.sess.Wait()
		_ = l.sess.Close()
		if l.release != nil {
			l.release()
		}
		l.cancel()
		if err != nil {
			if out := strings.TrimSpace(stripEscapes(l.Output())); out != "" {
				err = fmt.Errorf("auth login on %s failed: %w: %s", l.host, err, lastLine(out))
			} else {
				err = fmt.Errorf("auth login on %s failed: %w", l.host, err)
			}
		}
		l.waitErr = err
		close(l.done)
	})
	<-l.done
	return l.waitErr
}

// Close abandons an in-flight login, killing the remote process and freeing its
// channel. It is safe to call after Wait (where it is a no-op) and is the right
// cleanup for "the user navigated away" or "a newer login superseded this one".
func (l *AuthLogin) Close() error {
	l.cancel()
	return l.Wait()
}

// Output returns the bounded tail of everything the login process wrote, escapes
// and all — the raw material for diagnosing a login that failed for a reason the
// exit status doesn't explain.
func (l *AuthLogin) Output() string { return l.out.String() }

// loginURLPattern matches both OAuth URLs the CLI can print: claude.com/cai/... for
// the subscription flow and platform.claude.com/... for console. Terminal escapes
// must be stripped first — under a PTY the URL is wrapped in an OSC-8 hyperlink.
var loginURLPattern = regexp.MustCompile(`https://(?:claude\.com/cai/oauth|platform\.claude\.com/oauth)/[^\s"'<>]+`)

// escapePattern matches the two escape families the CLI emits: CSI sequences (color,
// cursor) and OSC strings (the hyperlink wrapper), the latter terminated by BEL or
// ST. Stripping both leaves the URL as plain text exactly once per emission.
var escapePattern = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]|\x1b\\][^\x07\x1b]*(?:\x07|\x1b\\\\)|\x1b[@-Z\\\\-_]")

func stripEscapes(s string) string { return escapePattern.ReplaceAllString(s, "") }

// findLoginURL pulls the OAuth URL out of already-stripped output, trimming the
// trailing punctuation a sentence can leave glued to it.
func findLoginURL(s string) string {
	m := loginURLPattern.FindString(s)
	return strings.TrimRight(m, ".,)]}'\"")
}

// lastLine returns the final non-empty line of s — for a failed login that is the
// CLI's "Login failed: …" message, which is what a user needs to see.
func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

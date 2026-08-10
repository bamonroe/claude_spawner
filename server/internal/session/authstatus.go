package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// `claude auth status --json` is the authoritative answer to "is this host's claude
// logged in, and as whom" — see docs/scopes/claude-auth-login.md. It is the pre-check
// before offering a login, the post-check that confirms one took, and later the
// explanation for a turn that failed on expired credentials.
//
// Two things about the CLI shape this wrapper. First, logged out is **exit 1 with
// valid JSON on stdout**: the non-zero status is expected, not an error, so the JSON
// is what we trust and the exit code is only a fallback. Second, which account it
// reports is purely a function of $HOME, so the identity being checked is chosen by
// running as the right user on the right host — not by any flag.

// DefaultAuthStatusTimeout bounds one status probe. It is a local credential-file
// read plus process startup, so it is fast; the ceiling only exists so an
// unreachable host can't pin a channel.
const DefaultAuthStatusTimeout = 30 * time.Second

// AuthStatus is the parsed `claude auth status --json` object. Every field but
// LoggedIn is absent when logged out.
type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`      // "claude.ai", "console", "none"
	APIProvider      string `json:"apiProvider"`     // "firstParty", …
	Email            string `json:"email,omitempty"` // the signed-in account
	OrgID            string `json:"orgId,omitempty"`
	OrgName          string `json:"orgName,omitempty"`
	SubscriptionType string `json:"subscriptionType,omitempty"` // "max", "pro", …
}

// Mode reports which login flow would refresh this identity, so a re-login can be
// started with the same billing identity the host already had rather than making the
// user re-pick. Unknown or logged-out states default to the subscription flow, which
// is the CLI's own default.
func (s AuthStatus) Mode() AuthLoginMode {
	if s.AuthMethod == "console" {
		return AuthLoginConsole
	}
	return AuthLoginClaudeAI
}

// Describe renders the status as one short sentence — the phrasing the client shows
// and TTS speaks, so it stays speakable rather than field-by-field.
func (s AuthStatus) Describe() string {
	if !s.LoggedIn {
		return "not logged in"
	}
	who := s.Email
	if who == "" {
		who = "an unnamed account"
	}
	out := "logged in as " + who
	if s.OrgName != "" {
		out += " (" + s.OrgName + ")"
	}
	if s.SubscriptionType != "" {
		out += ", " + s.SubscriptionType
	}
	return out
}

// ErrAuthStatusUnparsable reports that `claude auth status --json` produced nothing
// we could read — a binary too old to have the subcommand, a missing claude, or a
// host that answered with only shell noise. It is genuinely different from "logged
// out", which is a well-formed answer, so callers can tell "no credentials" apart
// from "can't ask".
var ErrAuthStatusUnparsable = errors.New("auth status: no JSON in output")

// CheckAuthStatus runs `claude auth status --json` on host and returns the parsed
// result. bin overrides the host's registered claude binary; empty defers to the
// registry the same way a turn does.
//
// A logged-out host is a successful call returning LoggedIn=false, not an error —
// the command's exit 1 is swallowed deliberately, because the JSON it prints
// alongside it is the answer. Only an unreachable host or unreadable output errors.
func CheckAuthStatus(ctx context.Context, pool *SSHPool, host, bin string) (AuthStatus, error) {
	var st AuthStatus
	if pool == nil {
		return st, errors.New("auth status: no ssh pool")
	}
	if host == "" {
		return st, errors.New("auth status: no host")
	}
	if bin == "" {
		bin = pool.binFor(host)
	}
	ctx, cancel := context.WithTimeout(ctx, DefaultAuthStatusTimeout)
	defer cancel()

	// `|| true` is the whole trick: exit 1 means logged out, and Run would otherwise
	// turn that expected answer into an error and discard the stdout we want. cd
	// $HOME so the command runs in the identity's own home, matching the login path.
	cmd := "cd \"$HOME\" && " + shellQuote(bin) + " auth status --json || true"
	out, err := pool.Run(ctx, host, cmd)
	if err != nil {
		return st, fmt.Errorf("auth status on %s: %w", host, err)
	}
	body := extractJSONObject(stripEscapes(string(out)))
	if body == "" {
		return st, fmt.Errorf("%w on %s: %s", ErrAuthStatusUnparsable, host, lastLine(strings.TrimSpace(string(out))))
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		return st, fmt.Errorf("auth status on %s: parse: %w", host, err)
	}
	return st, nil
}

// extractJSONObject pulls the outermost {...} out of command output. A login shell
// can prepend motd or rc-file chatter, so anchoring on the first brace and the last
// is more robust than assuming stdout is pure JSON.
func extractJSONObject(s string) string {
	i := strings.Index(s, "{")
	j := strings.LastIndex(s, "}")
	if i < 0 || j < i {
		return ""
	}
	return s[i : j+1]
}

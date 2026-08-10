package session

import "strings"

// Telling "this turn failed because the host's claude is logged out" apart from any
// other turn failure is what lets the client offer a re-login inline instead of
// printing a bare error and leaving the user to hunt through settings.
//
// The CLI has no machine-readable code for it — an expired OAuth token or a rejected
// API key surfaces as prose on stderr, which the driver wraps into the turn error. So
// the classifier is a substring match, deliberately narrow: every phrase below is one
// the CLI only emits for a credential problem. A miss just means the user gets the
// ordinary error (and can still re-login from Hosts settings), so we err toward
// under-matching rather than mislabeling an unrelated failure as "log in again".

var authFailurePhrases = []string{
	"oauth token has expired",
	"oauth token expired",
	"invalid api key",
	"authentication_error",
	"authentication failed",
	"please run /login",
	"please run `claude auth login`",
	"invalid bearer token",
	"credentials are invalid",
	"not logged in",
	"no credentials found",
}

// IsAuthFailure reports whether err (or any message text) reads as a Claude
// credential failure rather than an ordinary turn error. Matching is
// case-insensitive on the whole message, since the driver wraps the CLI's output in
// its own context.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	return textIsAuthFailure(err.Error())
}

func textIsAuthFailure(s string) bool {
	s = strings.ToLower(s)
	for _, p := range authFailurePhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

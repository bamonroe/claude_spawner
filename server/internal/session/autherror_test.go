package session

import (
	"errors"
	"fmt"
	"testing"
)

func TestIsAuthFailure(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("turn: claude exited 1: OAuth token has expired. Please run /login"), true},
		{fmt.Errorf("wrapped: %w", errors.New("Invalid API key · Please run /login")), true},
		{errors.New("API error: authentication_error"), true},
		{errors.New("context deadline exceeded"), false},
		{errors.New("no such file or directory"), false},
		{errors.New("http 401 unauthorized from a proxy"), false}, // too generic to claim a login problem
	}
	for _, c := range cases {
		if got := IsAuthFailure(c.err); got != c.want {
			t.Errorf("IsAuthFailure(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

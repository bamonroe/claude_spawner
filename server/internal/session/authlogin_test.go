package session

import (
	"strings"
	"testing"
)

// Under a PTY the CLI wraps the URL in an OSC-8 hyperlink, so the same URL appears
// twice with escape bytes around it — the exact shape the probe recorded. Stripping
// must leave a single clean URL.
func TestFindLoginURLThroughHyperlinkEscapes(t *testing.T) {
	url := "https://claude.com/cai/oauth/authorize?code=true&client_id=abc123&state=xyz"
	raw := "\x1b[2mOpening browser to sign in…\x1b[0m\r\n" +
		"If the browser didn't open, visit: \x1b]8;;" + url + "\x07\x1b[4m" + url + "\x1b[0m\x1b]8;;\x07\r\n" +
		"Paste code here if prompted > "
	got := findLoginURL(stripEscapes(raw))
	if got != url {
		t.Fatalf("url = %q, want %q", got, url)
	}
	if strings.ContainsRune(stripEscapes(raw), '\x1b') {
		t.Fatal("escapes survived stripping")
	}
}

func TestFindLoginURLConsoleFlow(t *testing.T) {
	url := "https://platform.claude.com/oauth/authorize?code=true&client_id=abc"
	if got := findLoginURL("If the browser didn't open, visit: " + url + "."); got != url {
		t.Fatalf("url = %q, want %q", got, url)
	}
}

func TestFindLoginURLIgnoresUnrelatedText(t *testing.T) {
	if got := findLoginURL("see https://claude.com/docs for help"); got != "" {
		t.Fatalf("matched unrelated url %q", got)
	}
}

func TestAuthLoginModeFlag(t *testing.T) {
	for _, tc := range []struct {
		mode AuthLoginMode
		want string
	}{
		{AuthLoginClaudeAI, "--claudeai"},
		{"", "--claudeai"},
		{AuthLoginConsole, "--console"},
	} {
		got, err := tc.mode.flag()
		if err != nil || got != tc.want {
			t.Fatalf("mode %q -> (%q, %v), want %q", tc.mode, got, err, tc.want)
		}
	}
	if _, err := AuthLoginMode("bogus").flag(); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestLastLineIsTheFailureReason(t *testing.T) {
	out := "Opening browser to sign in…\nPaste code here if prompted > \nLogin failed: Request failed with status code 400\n\n"
	if got := lastLine(out); got != "Login failed: Request failed with status code 400" {
		t.Fatalf("lastLine = %q", got)
	}
}

func TestStartAuthLoginValidatesInputs(t *testing.T) {
	if _, err := StartAuthLogin(t.Context(), nil, "h", "", AuthLoginClaudeAI, 0); err == nil {
		t.Fatal("expected error without a pool")
	}
	if _, err := StartAuthLogin(t.Context(), &SSHPool{}, "", "", AuthLoginClaudeAI, 0); err == nil {
		t.Fatal("expected error without a host")
	}
	if _, err := StartAuthLogin(t.Context(), &SSHPool{}, "h", "", "bogus", 0); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

package session

import "testing"

func TestExtractJSONObjectIgnoresShellNoise(t *testing.T) {
	in := "Welcome to host\n{\"loggedIn\": true}\n"
	if got := extractJSONObject(in); got != `{"loggedIn": true}` {
		t.Fatalf("extractJSONObject = %q", got)
	}
	if got := extractJSONObject("claude: command not found"); got != "" {
		t.Fatalf("want empty for non-JSON, got %q", got)
	}
}

func TestAuthStatusModeAndDescribe(t *testing.T) {
	out := AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", Email: "a@b.c", OrgName: "Acme", SubscriptionType: "max"}
	if out.Mode() != AuthLoginClaudeAI {
		t.Fatalf("mode = %q", out.Mode())
	}
	if got, want := out.Describe(), "logged in as a@b.c (Acme), max"; got != want {
		t.Fatalf("Describe = %q want %q", got, want)
	}
	consoleOut := AuthStatus{LoggedIn: true, AuthMethod: "console"}
	if consoleOut.Mode() != AuthLoginConsole {
		t.Fatalf("console mode = %q", consoleOut.Mode())
	}
	if got := (AuthStatus{}).Describe(); got != "not logged in" {
		t.Fatalf("logged-out Describe = %q", got)
	}
}

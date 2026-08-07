package session

import "testing"

func TestSuggestShellCommands(t *testing.T) {
	cat := []*ShellCommand{
		{Name: "disk space", Command: "df -h"},
		{Name: "restart caddy", Command: "systemctl restart caddy"},
		{Name: "uptime", Command: "uptime"},
	}
	got := SuggestShellCommands("disk spase", cat, 2)
	if len(got) != 1 || got[0] != "disk space" {
		t.Fatalf("near-miss suggestion = %v", got)
	}
	if got := SuggestShellCommands("make me a sandwich", cat, 2); len(got) != 0 {
		t.Fatalf("unrelated utterance suggested %v", got)
	}
	if got := SuggestShellCommands("uptim", cat, 2); len(got) != 1 || got[0] != "uptime" {
		t.Fatalf("single-word suggestion = %v", got)
	}
}

func TestMissingShellArgs(t *testing.T) {
	if got := MissingShellArgs("deploy $1 $2", []string{"staging"}); len(got) != 1 || got[0] != 2 {
		t.Fatalf("missing = %v", got)
	}
	if got := MissingShellArgs("deploy $1", []string{"staging"}); got != nil {
		t.Fatalf("supplied arg reported missing: %v", got)
	}
	if got := MissingShellArgs("uptime", nil); got != nil {
		t.Fatalf("template with no placeholders: %v", got)
	}
	if got := MissingShellArgs("echo $* $$", nil); got != nil {
		t.Fatalf("$* and $$ are not positional: %v", got)
	}
}

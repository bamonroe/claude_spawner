package session

import (
	"reflect"
	"testing"
)

func TestResolveShellCommand(t *testing.T) {
	cat := []*ShellCommand{
		{Name: "deploy", Command: "deploy.sh"},
		{Name: "deploy staging", Command: "deploy.sh staging"},
		{Name: "disk space", Command: "df -h"},
	}
	cases := []struct {
		name  string
		utter string
		want  string
		args  []string
		ok    bool
	}{
		{"exact", "deploy", "deploy", nil, true},
		{"multi word alias", "disk space", "disk space", nil, true},
		{"longest wins", "deploy staging now", "deploy staging", []string{"now"}, true},
		{"shorter alias keeps args", "deploy prod", "deploy", []string{"prod"}, true},
		{"punctuation and case", "Disk Space, please.", "disk space", []string{"please"}, true},
		{"word aligned", "deployment", "", nil, false},
		{"no match", "restart caddy", "", nil, false},
		{"empty utterance", "   ", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, args, ok := ResolveShellCommand(tc.utter, cat)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if got.Name != tc.want {
				t.Errorf("matched %q, want %q", got.Name, tc.want)
			}
			if !reflect.DeepEqual(args, tc.args) {
				t.Errorf("args = %#v, want %#v", args, tc.args)
			}
		})
	}
}

func TestResolveShellCommandEmptyCatalogue(t *testing.T) {
	if _, _, ok := ResolveShellCommand("deploy", nil); ok {
		t.Fatal("empty catalogue should not match")
	}
}

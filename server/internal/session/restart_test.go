package session

import "testing"

// The status file is append-only and the server may read it mid-write, so the
// parser must take the LAST phase line and ignore anything else in the file.
func TestLastRebuildPhase(t *testing.T) {
	cases := []struct {
		name, in, phase, errText string
	}{
		{"empty", "", "", ""},
		{"only started", "phase=started mode=build\n", "started", ""},
		{"last wins", "phase=started mode=build\nphase=finished mode=build\n", "finished", ""},
		{"failure carries reason", "phase=started mode=rebuild\nphase=failed mode=rebuild error=exited 1 (see log)\n", "failed", "exited 1 (see log)"},
		{"ignores noise", "==> build image\nphase=finished mode=bounce\n==> done.\n", "finished", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			phase, errText := lastRebuildPhase(tc.in)
			if phase != tc.phase || errText != tc.errText {
				t.Fatalf("got (%q, %q), want (%q, %q)", phase, errText, tc.phase, tc.errText)
			}
		})
	}
}

// Restart must report a terminal phase even with no pool (the local path), so the
// app's spinner always resolves.
func TestRestartReportsPhases(t *testing.T) {
	d := &Driver{RestartCmd: "true"}
	phases := make(chan string, 4)
	if err := d.Restart(t.Context(), "build", func(phase, errText string) { phases <- phase }); err != nil {
		t.Fatal(err)
	}
	if got := <-phases; got != "started" {
		t.Fatalf("first phase = %q, want started", got)
	}
	if got := <-phases; got != "finished" {
		t.Fatalf("second phase = %q, want finished", got)
	}
}

func TestRestartReportsFailure(t *testing.T) {
	d := &Driver{RestartCmd: "exit 3"}
	phases := make(chan string, 4)
	if err := d.Restart(t.Context(), "rebuild", func(phase, errText string) { phases <- phase }); err != nil {
		t.Fatal(err)
	}
	<-phases // started
	if got := <-phases; got != "failed" {
		t.Fatalf("second phase = %q, want failed", got)
	}
}

package gateway

import "testing"

// stripInjected must invert exactly the scaffolding dictate() appends, so history
// (read from Claude's transcript, which stores the augmented prompt) matches the
// raw text the live echo showed — otherwise the app can't dedupe the replayed
// turn and the hidden instructions leak into the chat view.
func TestStripInjected(t *testing.T) {
	const spoken = "what was the last thing you were working on"
	jobs := jobsInstruction("/home/bam/.spawner-jobs/spawner-job")
	notes := jobNotesPreamble([]string{"• `go build ./...` finished. Last output:\nok"})
	cases := map[string]string{
		"plain":            spoken,
		"brief":            spoken + briefSuffix,
		"ask":              spoken + askInstruction,
		"brief+ask":        spoken + briefSuffix + askInstruction,
		"seed":             seedPreamble("recap of the prior chat") + spoken,
		"seed+brief+ask":   seedPreamble("recap") + spoken + briefSuffix + askInstruction,
		"jobsInstr":        spoken + jobs,
		"ask+jobsInstr":    spoken + askInstruction + jobs,
		"jobNotes":         notes + spoken,
		"jobNotes+ask":     notes + spoken + askInstruction,
		"seedInsideNotes":  notes + seedPreamble("recap") + spoken,
		"notes+brief+jobs": notes + spoken + briefSuffix + jobs,
	}
	for name, augmented := range cases {
		if got := stripInjected(augmented); got != spoken {
			t.Errorf("%s: stripInjected did not recover the spoken text\n got: %q\nwant: %q", name, got, spoken)
		}
	}
	// A message with no scaffolding is returned untouched.
	if got := stripInjected(spoken); got != spoken {
		t.Errorf("plain text should be unchanged, got %q", got)
	}
}

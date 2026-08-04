package gateway

import (
	"strings"
	"testing"
)

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
	// The autonomous job-notify prompt is WHOLLY synthetic (envelope + notes +
	// instruction, no user words), so it must strip to empty — serveHistory then drops
	// the row so history matches the live view, where it never showed a user bubble.
	notify := jobNotifyPrompt([]string{"• `go build ./...` finished. Last output:\nok"})
	if got := stripInjected(notify); got != "" {
		t.Errorf("autonomous job-notify prompt should strip to empty, got %q", got)
	}
}

// A recap is a VERBATIM transcript of earlier turns, and those turns' prompts
// carried this same scaffolding — so the recap's interior holds real-looking
// copies of the suffix markers. Cutting at the first of them destroyed the recap's
// own closing marker, the recap strip then found no close and gave up, and the
// whole prior conversation rendered in the chat log as a user bubble (observed
// live on the emulator 2026-08-03, and in 1 of 8 recap prompts stored on the dev
// host). The prepended framed blocks must come off before the suffix trims.
func TestStripInjectedRecapQuotingScaffolding(t *testing.T) {
	const spoken = "what was the last thing you were working on"
	jobs := jobsInstruction("/home/bam/.spawner-jobs/spawner-job")
	notes := jobNotesPreamble([]string{"• `go build ./...` finished."})
	// The recap quotes earlier turns complete with their own scaffolding.
	quoted := "User: fix the build" + jobs + "\n\nAssistant: done\n\n" +
		"User: and the tests" + briefSuffix + jobs
	cases := map[string]string{
		"recapQuotesJobs":       seedPreamble(quoted) + spoken + jobs,
		"recapQuotesJobs+brief": seedPreamble(quoted) + spoken + briefSuffix + jobs,
		"notes+recapQuotesJobs": notes + seedPreamble(quoted) + spoken + jobs,
		"recapQuotesNotes":      seedPreamble(quoted+"\n\n"+notes+"User: ok") + spoken + jobs,
	}
	for name, augmented := range cases {
		got := stripInjected(augmented)
		if got != spoken {
			t.Errorf("%s: recap leaked instead of stripping\n got: %.300q\nwant: %q", name, got, spoken)
		}
	}
}

// The augmented prompt reaches stripInjected only after a round trip through a
// backend's transcript reader, and those readers reshape whitespace (opencode
// rejoins text parts with its own separator, antigravity trims its <USER_REQUEST>
// envelope). The strip must survive that drift: when it doesn't, the whole recap —
// the entire prior conversation — renders in the chat log as a user bubble.
func TestStripInjectedSurvivesWhitespaceDrift(t *testing.T) {
	const spoken = "what was the last thing you were working on"
	jobs := jobsInstruction("/home/bam/.spawner-jobs/spawner-job")
	notes := jobNotesPreamble([]string{"• `go build ./...` finished."})
	mangle := map[string]func(string) string{
		"leadingBlank":   func(s string) string { return "\n\n" + s },
		"trailingBlank":  func(s string) string { return s + "\n" },
		"collapsedBlank": func(s string) string { return strings.ReplaceAll(s, "\n\n", "\n") },
		"crlf":           func(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") },
	}
	augmented := map[string]string{
		"seed":            seedPreamble("User: hi\n\nAssistant: hello") + spoken,
		"seed+brief+jobs": seedPreamble("recap") + spoken + briefSuffix + jobs,
		"notes+seed":      notes + seedPreamble("recap") + spoken,
	}
	for shape, prompt := range augmented {
		for name, f := range mangle {
			got := stripInjected(f(prompt))
			if strings.TrimSpace(strings.ReplaceAll(got, "\r", "")) != spoken {
				t.Errorf("%s/%s: scaffolding leaked into history\n got: %q\nwant: %q", shape, name, got, spoken)
			}
		}
	}
	// Without a closing marker there's no way to tell where our block ends and the
	// user's words begin, so the message is left intact rather than truncated away.
	orphan := seedRecapOpen + "a recap with no close marker"
	if got := stripInjected(orphan); got != orphan {
		t.Errorf("an unterminated recap must be left untouched, got %q", got)
	}
}

package agent

import (
	"fmt"
	"io"
	"strings"
)

// The Antigravity (agy) backend, self-contained: registry entry, per-turn command
// line, and the plain-text stdout reader. Antigravity is Google's Gemini-powered
// agentic CLI; unlike the other backends it has no machine-readable stream mode —
// `agy --prompt` runs one turn non-interactively and prints the final response as
// plain prose on stdout, which is exactly the speakable reply we want. Rich turn
// events and token accounting are therefore not available here: agy writes a
// structured transcript.jsonl on disk (see TranscriptAntigravity), but it records
// no token usage and is keyed by an internal id we don't hold, so this backend
// surfaces the reply only. When agy grows a `--output-format json` mode (or a
// resolvable transcript path), swap parseAgyText for a real stream parser and wire
// a transcript reader.
//
// One thing IS reconstructed from that on-disk transcript today: `agy --print`
// concatenates a turn's several assistant messages into one blank-line-less blob on
// stdout, so after the turn the driver reads the transcript back and restores the
// paragraph breaks (session.reconstructAgyReply). parseAgyText still produces the
// fallback/live reply; the driver only rewrites its line breaks.

// agyPrintTimeout caps agy's own non-interactive wait. Its default is 5m, too
// short for a real agentic turn; the gateway's context still governs cancellation,
// this just keeps agy from self-aborting a long turn before we do.
const agyPrintTimeout = "45m"

// antigravity builds the Antigravity backend entry. NOTE: the --conversation resume
// below is currently a no-op — recent agy ignores a caller-supplied conversation id
// ("not found, ignoring --conversation flag") and keys its store by an internal id
// of its own, so turns do NOT actually resume the same conversation and there is no
// stable id to key a history reader on (see TODO.md; reconstructAgyReply works
// around this by content-matching the transcript instead of by id). SelfAssignsID
// stays false regardless. agy ignores the process cwd and
// works in its own scratch project unless a workspace is named, so every turn
// passes the session's directory via --add-dir. Models are agy's display strings
// (what `agy --model` accepts verbatim), fronted by short spoken aliases.
func antigravity() *Agent {
	return &Agent{
		ID:           "antigravity",
		Name:         "Antigravity",
		Bin:          "agy",
		Transcript:   TranscriptAntigravity,
		DefaultModel: "gemini-pro",
		Models: []Model{
			// Flags are agy's exact `agy models` display strings — the value agy's
			// --model accepts. Aliases/spoken forms keep them sayable over voice; the
			// awkward parenthesised names are why ordinal selection ("use model 2")
			// matters here.
			{Alias: "gemini-pro", Flag: "Gemini 3.1 Pro (High)", Spoken: []string{"pro", "gemini pro", "gemini three pro"}},
			{Alias: "gemini-pro-low", Flag: "Gemini 3.1 Pro (Low)", Spoken: []string{"pro low", "pro fast"}},
			{Alias: "gemini-flash", Flag: "Gemini 3.5 Flash (High)", Spoken: []string{"flash", "gemini flash"}},
			{Alias: "gemini-flash-med", Flag: "Gemini 3.5 Flash (Medium)", Spoken: []string{"flash medium"}},
			{Alias: "gemini-flash-low", Flag: "Gemini 3.5 Flash (Low)", Spoken: []string{"flash low", "fast"}},
		},
		build: func(a *Agent, s TurnSpec, m Model) []string {
			// --conversation carries our own id on every turn: the first creates it,
			// later turns resume it (agy has no separate "resume" verb). agy works in
			// its private scratch project unless we name the workspace, so --add-dir
			// points it at the session directory.
			args := []string{"--conversation", s.SessionID}
			if s.Dir != "" {
				args = append(args, "--add-dir", s.Dir)
			}
			if s.Bypass {
				args = append(args, "--dangerously-skip-permissions")
			}
			if m.Flag != "" {
				args = append(args, "--model", m.Flag)
			}
			args = append(args, "--print-timeout", agyPrintTimeout)
			// --prompt (alias of --print) triggers non-interactive mode and takes the
			// dictation as its value. The =form keeps a prompt that starts with "-"
			// from being misparsed as a flag.
			args = append(args, "--prompt="+s.Prompt)
			return args
		},
		ParseTurn: parseAgyText,
	}
}

// parseAgyText reads agy's non-interactive stdout — plain prose, the whole of it
// the final response — into the turn reply. There are no per-event callbacks to
// fan out (agy emits no structured stream) and no token usage to report; OnText
// gets the reply once so the caller can render it. A non-zero exit is caught by
// the driver's proc.Wait, so an empty reply here means agy printed nothing on a
// clean exit, which we treat as a failed turn (mirroring the other parsers'
// "stream ended without a result").
func parseAgyText(r io.Reader, cb TurnCallbacks) (TurnResult, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return TurnResult{}, fmt.Errorf("read agy output: %w", err)
	}
	reply := strings.TrimSpace(string(b))
	if reply == "" {
		return TurnResult{}, fmt.Errorf("agy produced no response")
	}
	if cb.OnText != nil {
		cb.OnText(reply)
	}
	return TurnResult{Reply: reply}, nil
}

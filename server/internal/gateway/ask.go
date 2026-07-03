package gateway

import (
	"encoding/json"
	"strings"
)

// askQuestion is one clarification Claude asks mid-task in interactive mode.
// Options (if any) are multiple-choice; empty means a free-text answer.
type askQuestion struct {
	Q       string   `json:"q"`
	Options []string `json:"options,omitempty"`
}

// askInstruction is appended to dictation in interactive mode. Headless `claude
// -p` can't prompt interactively, so we ask Claude to emit a machine-readable
// block instead; the server detects it and forwards it to the app as an `ask`.
const askInstruction = "\n\n[Interactive mode] If you need information from me before you can " +
	"proceed correctly, do NOT guess — reply with ONLY this block and nothing else:\n" +
	"::ASK::\n" +
	`[{"q":"your question","options":["choice A","choice B"]}]` + "\n" +
	"::END::\n" +
	`Include "options" only for multiple-choice questions; omit it for free-text ones. ` +
	"Ask only when genuinely blocked; otherwise proceed with sensible defaults and don't ask."

// parseAsk extracts a ::ASK:: [...] ::END:: block from a turn reply. Lenient:
// tolerates prose around the block. Returns false (so the reply is shown as a
// normal answer) if there's no block, it's malformed, or it has no real question.
func parseAsk(reply string) ([]askQuestion, bool) {
	start := strings.Index(reply, "::ASK::")
	if start < 0 {
		return nil, false
	}
	rest := reply[start+len("::ASK::"):]
	end := strings.Index(rest, "::END::")
	if end < 0 {
		return nil, false
	}
	var qs []askQuestion
	if json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &qs) != nil {
		return nil, false
	}
	out := make([]askQuestion, 0, len(qs))
	for _, q := range qs {
		if strings.TrimSpace(q.Q) != "" {
			out = append(out, q)
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

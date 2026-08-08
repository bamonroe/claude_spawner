package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realExport is a trimmed but real-shaped `opencode export` payload: a user turn,
// an assistant turn that was tool-only (no prose), then an assistant turn with
// prose. Token accounting lives on step-finish parts; the session-level
// info.tokens is intentionally larger (summed across turns) to prove the reader
// ignores it in favor of the last step-finish.
const realExport = `{
  "info": { "id": "ses_x", "tokens": { "input": 4831, "output": 50 } },
  "messages": [
    { "info": { "id": "msg_u1", "role": "user", "time": { "created": 1783980863951 } },
      "parts": [ { "type": "text", "text": "reply hi then stop" } ] },
    { "info": { "id": "msg_a0", "role": "assistant", "time": { "created": 1783980865000 } },
      "parts": [
        { "type": "step-start" },
        { "type": "tool", "tool": "task" },
        { "type": "step-finish", "tokens": { "input": 2050, "output": 32, "reasoning": 0, "cache": { "read": 0, "write": 0 } } }
      ] },
    { "info": { "id": "msg_a1", "role": "assistant", "time": { "created": 1783980870000 } },
      "parts": [
        { "type": "step-start" },
        { "type": "text", "text": "Hi." },
        { "type": "text", "text": "(internal)", "synthetic": true },
        { "type": "step-finish", "tokens": { "input": 2781, "output": 18, "reasoning": 2, "cache": { "read": 40, "write": 5 } } }
      ] }
  ]
}`

func parseExport(t *testing.T) opencodeExport {
	t.Helper()
	var ex opencodeExport
	if err := json.Unmarshal([]byte(realExport), &ex); err != nil {
		t.Fatal(err)
	}
	return ex
}

// TestExportMessages checks the export → conversation mapping: user + assistant
// roles map through (assistant → "claude"), a tool-only assistant turn is dropped
// from the replay, synthetic text is skipped, and the surviving assistant turn
// carries its last step-finish usage (reasoning folded into Output).
func TestExportMessages(t *testing.T) {
	msgs := exportMessages(parseExport(t))
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2 (tool-only assistant turn dropped): %+v", len(msgs), msgs)
	}
	// Index is assigned by readTranscriptChain across the whole chain, not here.
	if msgs[0].Role != "user" || msgs[0].Text != "reply hi then stop" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	// The durable msg_… id is carried through (the dropped tool-only turn's id is
	// gone with it, so the surviving prose assistant turn keeps its own id).
	if msgs[0].ID != "msg_u1" {
		t.Errorf("msg0 ID = %q, want msg_u1", msgs[0].ID)
	}
	if msgs[1].ID != "msg_a1" {
		t.Errorf("msg1 ID = %q, want msg_a1", msgs[1].ID)
	}
	if msgs[0].Usage != nil {
		t.Errorf("user turn should carry no usage, got %+v", msgs[0].Usage)
	}
	if msgs[1].Role != "claude" || msgs[1].Text != "Hi." {
		t.Errorf("msg1 = %+v (synthetic text should be skipped)", msgs[1])
	}
	if msgs[1].Ts != 1783980870 {
		t.Errorf("msg1 ts = %d, want ms→s conversion 1783980870", msgs[1].Ts)
	}
	want := Usage{Input: 2781, Output: 20, CacheRead: 40, CacheWrite: 5}
	if msgs[1].Usage == nil || *msgs[1].Usage != want {
		t.Errorf("msg1 usage = %+v, want %+v", msgs[1].Usage, want)
	}
}

// TestExportContext confirms the context snapshot is the LAST step-finish's
// tokens (the newest turn's full prompt), not the summed session-level
// info.tokens — so the reattach context meter matches the live one.
func TestExportContext(t *testing.T) {
	cx := exportContext(parseExport(t))
	if cx == nil {
		t.Fatal("want a context snapshot")
	}
	want := Usage{Input: 2781, Output: 20, CacheRead: 40, CacheWrite: 5}
	if cx.Usage != want {
		t.Errorf("context usage = %+v, want the last step-finish %+v (not summed info.tokens)", cx.Usage, want)
	}
	if cx.At != 1783980870 {
		t.Errorf("context at = %d, want 1783980870", cx.At)
	}
}

// TestValidOpencodeID gates the ids interpolated into remote shell commands: only
// `ses_`+alphanumerics pass, so a malformed or injection-bearing id is rejected.
func TestValidOpencodeID(t *testing.T) {
	good := []string{"ses_0a2744e61ffe86lSFnc0BVot51", "ses_abc123"}
	bad := []string{"", "ses_", "abc", "ses_bad-id", "ses_a b", "ses_a;rm -rf", "ses_a/b"}
	for _, id := range good {
		if !validOpencodeID(id) {
			t.Errorf("validOpencodeID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if validOpencodeID(id) {
			t.Errorf("validOpencodeID(%q) = true, want false", id)
		}
	}
}

// opencode stores a CLI-supplied user message wrapped in its own double quotes.
// Leaving them on breaks every exact-text consumer downstream (the gateway's
// scaffolding strip, the app's live-vs-history dedupe), so the reader unwraps it.
func TestExportMessagesUnwrapsOpencodeQuotes(t *testing.T) {
	const quoted = `{"messages":[
    {"info":{"id":"msg_u1","role":"user","time":{"created":1000}},
     "parts":[{"type":"text","text":"\"do the next todo\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)\""}]},
    {"info":{"id":"msg_a1","role":"assistant","time":{"created":2000}},
     "parts":[{"type":"text","text":"\"quoted assistant prose stays\""}]}
  ]}`
	var ex opencodeExport
	if err := json.Unmarshal([]byte(quoted), &ex); err != nil {
		t.Fatal(err)
	}
	msgs := exportMessages(ex)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	want := "do the next todo\n\n(Reply briefly, in plain sentences suitable for text-to-speech.)"
	if msgs[0].Text != want {
		t.Errorf("user text = %q, want %q", msgs[0].Text, want)
	}
	// Only the user message carries opencode's argv quoting; assistant prose is
	// the model's own words and must not be rewritten.
	if msgs[1].Text != `"quoted assistant prose stays"` {
		t.Errorf("assistant text was rewritten: %q", msgs[1].Text)
	}
}

// opencode stores a user prompt as the quoted CLI argument it received
// (`"…"`). The reader must hand back the user's own text: the quotes break both
// the gateway's scaffolding strip and the app's live-vs-history dedupe, which
// showed the message twice — once quoted with the "(Reply briefly…)" hint still
// attached.
func TestExportMessagesUnquotesUserText(t *testing.T) {
	cases := map[string]string{
		`"can you use the todo skill"`: "can you use the todo skill",
		`"he said "hi" to me"`:         `he said "hi" to me`,
		"unquoted prompt":              "unquoted prompt",
		`"unterminated`:                `"unterminated`,
		`say "yes"`:                    `say "yes"`,
	}
	for in, want := range cases {
		if got := opencodeUnquote(in); got != want {
			t.Errorf("opencodeUnquote(%q) = %q, want %q", in, got, want)
		}
	}

	ex := parseExport(t)
	for _, m := range exportMessages(ex) {
		if m.Role == "user" && strings.HasPrefix(m.Text, `"`) && strings.HasSuffix(m.Text, `"`) {
			t.Errorf("user message still wrapped in opencode's quotes: %q", m.Text)
		}
	}
}

// chainSig lets a digest be cached instead of re-exporting the whole chain, so it
// must (a) hold steady while the opencode store is untouched, (b) change when a
// turn lands — including one that only reaches the write-ahead log — and (c) opt
// out honestly when there is no store to describe.
func TestOpencodeChainSig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var fs opencodeFS // remote nil = local, per the hermetic-test path
	ids := []string{"ses_a", "ses_b"}

	if _, ok := fs.chainSig(context.Background(), ids); ok {
		t.Fatal("chainSig reported a signature with no opencode store on disk")
	}

	store := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(store, "opencode.db")
	if err := os.WriteFile(db, []byte("sqlite"), 0o644); err != nil {
		t.Fatal(err)
	}
	sig, ok := fs.chainSig(context.Background(), ids)
	if !ok || sig == "" {
		t.Fatalf("chainSig on an existing store = %q, ok %v", sig, ok)
	}
	if again, _ := fs.chainSig(context.Background(), ids); again != sig {
		t.Errorf("chainSig changed with no write: %q then %q", sig, again)
	}
	if other, _ := fs.chainSig(context.Background(), []string{"ses_a"}); other == sig {
		t.Error("chainSig ignored the id chain: a shorter chain signed the same")
	}

	// A turn that only reaches the -wal must still move the signature; signing the
	// database alone would call this chain unchanged.
	if err := os.WriteFile(filepath.Join(store, "opencode.db-wal"), []byte("pending"), 0o644); err != nil {
		t.Fatal(err)
	}
	if withWAL, _ := fs.chainSig(context.Background(), ids); withWAL == sig {
		t.Error("chainSig unchanged after a write landed in the -wal")
	}
}

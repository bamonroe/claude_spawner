// Client↔server wire drift checks: the Kotlin client (shared by the Android
// app and the browser client) hand-writes its wire strings in one file,
// android/app/src/commonMain/kotlin/com/bam/spawner/net/Protocol.kt — every
// outbound builder is a `put("type", "<x>")` in the Outbound object, and every
// inbound parse is a `"<x>" ->` branch in ServerMsg.parse. These tests extract
// both sets by regex and cross-check them against the Go gateway (the AST
// extraction shared with docsync_test.go), so the two ends of the protocol
// cannot drift without a red `go test ./...`:
//
//   - a type the client sends must have a server handler (and vice versa);
//   - a type the server emits must have a client parse branch (and vice versa);
//   - the audio codec constants must agree byte-for-byte on both sides.
//
// A deliberately one-sided message goes in the exemption map next to the
// check, with a reason — that keeps "the app doesn't use this" a recorded
// decision instead of silent drift.
package docsync

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const protocolKt = "android/app/src/commonMain/kotlin/com/bam/spawner/net/Protocol.kt"

// clientSentTypes extracts the `put("type", "<x>")` literals from the Outbound
// builders in Protocol.kt.
func clientSentTypes(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readDoc(t, root, filepath.FromSlash(protocolKt))
	re := regexp.MustCompile(`put\("type",\s*"([a-z0-9_]+)"\)`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("found no put(\"type\", ...) literals in Protocol.kt — extractor broken?")
	}
	return out
}

// clientParsedTypes extracts the `"<x>" ->` branches of ServerMsg.parse's
// when-block in Protocol.kt.
func clientParsedTypes(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readDoc(t, root, filepath.FromSlash(protocolKt))
	re := regexp.MustCompile(`(?m)^\s*"([a-z0-9_]+)"\s*->`)
	out := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("found no \"type\" -> parse branches in Protocol.kt — extractor broken?")
	}
	return out
}

func diffSets(got map[string]bool, want map[string]bool, exempt map[string]string) []string {
	var missing []string
	for k := range got {
		if !want[k] && exempt[k] == "" {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	return missing
}

func TestClientSendsOnlyHandledTypes(t *testing.T) {
	// Client-sent types handled outside the wireHandlers dispatch table.
	exempt := map[string]string{
		"hello": "the auth handshake is consumed before dispatch (gateway.go serve loop)",
	}
	root := repoRoot(t)
	sent := clientSentTypes(t, root)
	handled := map[string]bool{}
	for _, s := range serverInboundTypes(t, root) {
		handled[s] = true
	}
	if bad := diffSets(sent, handled, exempt); len(bad) > 0 {
		t.Errorf("Protocol.kt Outbound builds type(s) the gateway has no wireHandlers entry for: %s\n"+
			"Add the handler in server/internal/gateway (and a docs/protocol.md row), or remove the builder.",
			strings.Join(bad, ", "))
	}
}

func TestServerHandlesOnlyClientSentTypes(t *testing.T) {
	// Inbound types the server accepts that no client builder produces — each
	// needs a reason, or the handler is dead surface a client change abandoned.
	exempt := map[string]string{
		"reply":         "alias of utterance kept for dialog replies from older clients",
		"list_sessions": "quiet session_list fetch; current clients use discover for the sidebar",
		"rename":        "name-keyed rename; current clients rename by id via rename_discovered",
		"delete":        "name-keyed delete; current clients delete by id via delete_discovered",
		"clear":         "voice-command path only (\"hey buddy, clear context\" arrives as utterance)",
		"compress":      "voice-command path only (arrives as utterance)",
		"cancel":        "voice-command path only (dialog cancel arrives as utterance)",
		"ping":          "app-level keepalive for clients that can't use WebSocket ping frames; current clients don't need it",
	}
	root := repoRoot(t)
	sent := clientSentTypes(t, root)
	for _, s := range serverInboundTypes(t, root) {
		if !sent[s] && exempt[s] == "" {
			t.Errorf("gateway handles inbound %q but Protocol.kt has no Outbound builder for it — "+
				"add the builder (or an exemption here with a reason)", s)
		}
	}
	for e := range exempt {
		if sent[e] {
			t.Errorf("exemption %q is stale: Protocol.kt now builds it — remove the exemption", e)
		}
	}
}

func TestClientParsesAllServerTypes(t *testing.T) {
	// Outbound types the server emits that the client deliberately ignores —
	// each needs a reason, or a new server message silently no-ops on the app.
	exempt := map[string]string{
		"session_list": "sidebar is fed by discovered (the superset); list refreshes ride along unparsed",
		"pong":         "reply to the app-level ping keepalive, which current clients don't send",
	}
	root := repoRoot(t)
	parsed := clientParsedTypes(t, root)
	for _, s := range serverOutboundTypes(t, root) {
		if !parsed[s] && exempt[s] == "" {
			t.Errorf("server emits %q but ServerMsg.parse has no branch for it — "+
				"add the branch in Protocol.kt (or an exemption here with a reason)", s)
		}
	}
	for e := range exempt {
		if parsed[e] {
			t.Errorf("exemption %q is stale: ServerMsg.parse now handles it — remove the exemption", e)
		}
	}
}

func TestClientParsesOnlyServerTypes(t *testing.T) {
	root := repoRoot(t)
	parsed := clientParsedTypes(t, root)
	emitted := map[string]bool{}
	for _, s := range serverOutboundTypes(t, root) {
		emitted[s] = true
	}
	if bad := diffSets(parsed, emitted, nil); len(bad) > 0 {
		t.Errorf("ServerMsg.parse handles type(s) the server never emits: %s\n"+
			"Either the server dropped the message (remove the branch) or messages.go builds it outside msg* constructors.",
			strings.Join(bad, ", "))
	}
}

// TestAudioCodecsAgree pins the audio codec strings on both sides of the wire
// and in the docs: the Go constants in gateway/audio.go, the Kotlin constants
// in Protocol.kt's Codecs object, and backticked mentions in protocol.md.
func TestAudioCodecsAgree(t *testing.T) {
	root := repoRoot(t)

	goSrc := readDoc(t, root, filepath.FromSlash("server/internal/gateway/audio.go"))
	goRe := regexp.MustCompile(`codec[A-Za-z0-9]+\s*=\s*"([a-z0-9_]+)"`)
	goSet := map[string]bool{}
	for _, m := range goRe.FindAllStringSubmatch(goSrc, -1) {
		goSet[m[1]] = true
	}
	if len(goSet) == 0 {
		t.Fatal("found no codec constants in gateway/audio.go — extractor broken?")
	}

	ktSrc := readDoc(t, root, filepath.FromSlash(protocolKt))
	ktObj := regexp.MustCompile(`(?s)object Codecs \{(.*?)\}`).FindStringSubmatch(ktSrc)
	if ktObj == nil {
		t.Fatal("Protocol.kt has no `object Codecs { ... }` — the client-side codec constants moved?")
	}
	ktRe := regexp.MustCompile(`const val [A-Z0-9_]+\s*=\s*"([a-z0-9_]+)"`)
	ktSet := map[string]bool{}
	for _, m := range ktRe.FindAllStringSubmatch(ktObj[1], -1) {
		ktSet[m[1]] = true
	}

	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	for c := range goSet {
		if !ktSet[c] {
			t.Errorf("codec %q is defined in gateway/audio.go but not in Protocol.kt's Codecs object", c)
		}
		if !strings.Contains(doc, `"`+c+`"`) && !strings.Contains(doc, "`"+c+"`") {
			t.Errorf("codec %q is not documented in docs/protocol.md", c)
		}
	}
	for c := range ktSet {
		if !goSet[c] {
			t.Errorf("codec %q is defined in Protocol.kt's Codecs but not in gateway/audio.go", c)
		}
	}
}

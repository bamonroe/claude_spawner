package docsync

// Payload *field name* drift checks: the JSON keys the gateway reads and writes
// (struct json tags + map[string]any keys in messages.go, plus the session-
// package structs marshaled straight onto the wire) must each be documented in
// docs/protocol.md — and, the other way, every field named in the protocol
// tables' payload column must exist in the code. Message *types* are covered by
// docsync_test.go; this file covers the fields inside them.

import (
	"go/ast"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// structJSONTags returns the json tag names (sans options like ",omitempty") of
// the named structs in the Go file at path. Empty names = every struct in the
// file. Missing a named struct is a test failure, so a rename can't silently
// drop a check.
func structJSONTags(t *testing.T, path string, names ...string) []string {
	t.Helper()
	_, f := parseGo(t, path)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = false
	}
	var tags []string
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		if len(names) > 0 {
			if _, wanted := want[ts.Name.Name]; !wanted {
				return true
			}
			want[ts.Name.Name] = true
		}
		for _, fld := range st.Fields.List {
			if fld.Tag == nil {
				continue
			}
			raw := strings.Trim(fld.Tag.Value, "`")
			tag := reflect.StructTag(raw).Get("json")
			if tag == "" || tag == "-" {
				continue
			}
			if i := strings.IndexByte(tag, ','); i >= 0 {
				tag = tag[:i]
			}
			if tag != "" {
				tags = append(tags, tag)
			}
		}
		return true
	})
	for name, found := range want {
		if !found {
			t.Fatalf("struct %s not found in %s — renamed? update fieldsync_test.go", name, path)
		}
	}
	return tags
}

// messagesGoWireKeys returns every JSON key messages.go can put on the wire:
// the string keys of map[string]any composite literals (the msg* builders) and
// of m["key"] = … index assignments (the optional fields). Keys of other map
// types (e.g. the spokenError map[string]string) are not wire fields and are
// skipped.
func messagesGoWireKeys(t *testing.T, root string) []string {
	t.Helper()
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "gateway", "messages.go"))
	seen := map[string]bool{}
	var keys []string
	add := func(k string) {
		if k != "" && !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	isMapStringAny := func(e ast.Expr) bool {
		mt, ok := e.(*ast.MapType)
		if !ok {
			return false
		}
		v, ok := mt.Value.(*ast.Ident)
		return ok && v.Name == "any"
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			if !isMapStringAny(x.Type) {
				return true
			}
			for _, elt := range x.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					add(strLit(kv.Key))
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range x.Lhs {
				if ix, ok := lhs.(*ast.IndexExpr); ok {
					add(strLit(ix.Index))
				}
			}
		}
		return true
	})
	if len(keys) == 0 {
		t.Fatal("found no wire map keys in messages.go — parser broken?")
	}
	return keys
}

// wireFieldSet is every JSON field name the gateway can read or write: the
// inbound struct + view structs in messages.go, the msg* map keys, the ask
// payload, and the session-package structs marshaled directly onto the wire
// (Host in host_list/host_put, Usage in output/attached/history, Message in
// history). "type" is the envelope, documented in prose, and excluded.
func wireFieldSet(t *testing.T, root string) map[string]bool {
	t.Helper()
	gw := filepath.Join(root, "server", "internal", "gateway")
	sess := filepath.Join(root, "server", "internal", "session")
	ag := filepath.Join(root, "server", "internal", "agent")
	set := map[string]bool{}
	for _, group := range [][]string{
		structJSONTags(t, filepath.Join(gw, "messages.go")), // inbound + the view structs
		messagesGoWireKeys(t, root),
		structJSONTags(t, filepath.Join(gw, "ask.go"), "askQuestion"),
		structJSONTags(t, filepath.Join(sess, "hosts.go"), "Host"),
		structJSONTags(t, filepath.Join(ag, "turn.go"), "Usage"), // aliased as session.Usage
		structJSONTags(t, filepath.Join(sess, "transcript.go"), "Message"),
	} {
		for _, f := range group {
			if f != "type" {
				set[f] = true
			}
		}
	}
	return set
}

// docHasField reports whether protocol.md mentions the field, in any of the
// forms the doc uses: quoted (`"name"`, `"name?"`) or backticked (`name`).
func docHasField(doc, f string) bool {
	return strings.Contains(doc, `"`+f+`"`) ||
		strings.Contains(doc, `"`+f+`?"`) ||
		strings.Contains(doc, "`"+f+"`")
}

// TestWireFieldsDocumented: every JSON field the gateway reads or writes must
// appear in docs/protocol.md.
func TestWireFieldsDocumented(t *testing.T) {
	root := repoRoot(t)
	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	var missing []string
	for f := range wireFieldSet(t, root) {
		if !docHasField(doc, f) {
			missing = append(missing, f)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/protocol.md is missing %d payload field(s): %s\n"+
			"Document each in the relevant message's payload column (quoted or backticked).",
			len(missing), strings.Join(missing, ", "))
	}
}

var (
	// A field name in a payload cell: quoted ("name", "name?") or backticked
	// (`name`). Value literals ("codex", "await_dir", …) are quoted too — they
	// are told apart by position: a value always follows a ':' or a '|'
	// alternation, a field never does.
	quotedField    = regexp.MustCompile(`"([a-z][a-z0-9_]*)\??"`)
	backtickField  = regexp.MustCompile("`([a-z][a-z0-9_]*)`")
	escapedPipeSub = "\x00" // stand-in for the tables' escaped \| while splitting cells
)

// payloadCells returns the payload column (2nd cell) of every row of the two
// message tables in protocol.md — the "App -> server" and "Server -> app"
// sections — skipping header and separator rows. The error-code table and the
// prose/example blocks are deliberately out of scope: their quoted tokens are
// codes and sample values, not payload fields.
func payloadCells(t *testing.T, doc string) []string {
	t.Helper()
	var cells []string
	inTable := false
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "## ") {
			inTable = strings.Contains(line, "App -> server") || strings.Contains(line, "Server -> app")
			continue
		}
		if !inTable || !strings.HasPrefix(line, "|") {
			continue
		}
		parts := strings.Split(strings.ReplaceAll(line, `\|`, escapedPipeSub), "|")
		if len(parts) < 3 {
			continue
		}
		head := strings.TrimSpace(parts[1])
		if head == "type" || strings.HasPrefix(head, "-") {
			continue // header / separator row
		}
		cells = append(cells, strings.ReplaceAll(parts[2], escapedPipeSub, `\|`))
	}
	if len(cells) == 0 {
		t.Fatal("found no payload cells in docs/protocol.md — table format changed?")
	}
	return cells
}

// docPayloadFields extracts the field names the protocol tables document.
func docPayloadFields(t *testing.T, doc string) []string {
	t.Helper()
	seen := map[string]bool{}
	var fields []string
	add := func(f string) {
		f = strings.TrimSuffix(f, "?")
		if !seen[f] {
			seen[f] = true
			fields = append(fields, f)
		}
	}
	for _, cell := range payloadCells(t, doc) {
		for _, m := range quotedField.FindAllStringSubmatchIndex(cell, -1) {
			// Skip value position: the previous non-space character is ':' (a key's
			// value) or '\'/'|' (an alternation like "user"\|"claude").
			i := m[0] - 1
			for i >= 0 && cell[i] == ' ' {
				i--
			}
			if i >= 0 && (cell[i] == ':' || cell[i] == '\\' || cell[i] == '|') {
				continue
			}
			add(cell[m[2]:m[3]])
		}
		for _, m := range backtickField.FindAllStringSubmatch(cell, -1) {
			add(m[1])
		}
	}
	return fields
}

// TestDocFieldsExistInCode: every field named in the protocol tables' payload
// column must be a field the gateway actually reads or writes — a doc typo or
// a field removed from the code fails here.
func TestDocFieldsExistInCode(t *testing.T) {
	root := repoRoot(t)
	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	code := wireFieldSet(t, root)
	var unknown []string
	for _, f := range docPayloadFields(t, doc) {
		if !code[f] {
			unknown = append(unknown, f)
		}
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		t.Errorf("docs/protocol.md documents %d payload field(s) the gateway never reads/writes: %s\n"+
			"Fix the doc, or add the field to the wire code.",
			len(unknown), strings.Join(unknown, ", "))
	}
}

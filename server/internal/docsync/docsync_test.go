// Package docsync enforces that the root-level documentation stays in sync
// with the code that is its source of truth. There are no non-test files here:
// the package exists solely so `go test ./...` (run before every commit) fails
// loudly when a code-derived fact drifts out of the docs.
//
// What it guards (see the "Documentation map" in CLAUDE.md for the full owner
// table):
//
//   - SPAWNER_* env vars read in internal/config  -> must be in CLAUDE.md
//   - inbound wire message types (gateway dispatch) -> must be in docs/protocol.md
//   - outbound wire message types (messages.go)     -> must be in docs/protocol.md
//   - msgError(code, ...) codes (internal/gateway)  -> must be in docs/protocol.md
//
// The command list has its own drift test (internal/command, registry <->
// docs/commands.json); this package covers the facts that previously had a
// prose copy with nothing tying it to the code.
//
// Each check requires the token to appear **backticked** (e.g. `hello_ok`) in
// the doc, which is how the protocol/config tables render them — this avoids a
// bare word in prose accidentally satisfying the check.
//
// Caching caveat: `go test` keys its cache on Go-source inputs, not on the
// Markdown files these tests read. A code change (a new message/env var/error
// code) always busts the cache and re-runs these checks — that's the main drift
// vector and it's covered. A *doc-only* deletion can be masked by a cached pass,
// so the canonical drift check runs uncached: `go test ./... -count=1`.
package docsync

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// envKey matches a bare SPAWNER_* environment-variable name in full — so error
// message strings that merely start with "SPAWNER_" (e.g. "SPAWNER_ROOT %q")
// are not mistaken for config keys.
var envKey = regexp.MustCompile(`^SPAWNER_[A-Z0-9_]+$`)

// repoRoot returns the repository root (…/claude_spawner), derived from this
// test file's location (…/server/internal/docsync/docsync_test.go).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

func readDoc(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func parseGo(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// strLit returns the string value of a basic string literal expression, or ""
// if the expression is not a string literal.
func strLit(e ast.Expr) string {
	bl, ok := e.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return ""
	}
	// Unquote is safe for a well-formed Go string literal.
	if s, err := strconv.Unquote(bl.Value); err == nil {
		return s
	}
	return ""
}

// reportMissing fails the test listing every token absent from doc, with a
// pointer to how to fix it.
func reportMissing(t *testing.T, doc, docName string, tokens []string, howToFix string) {
	t.Helper()
	var missing []string
	for _, tok := range tokens {
		if !strings.Contains(doc, "`"+tok+"`") {
			missing = append(missing, tok)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%s is missing %d documented item(s): %s\n%s",
			docName, len(missing), strings.Join(missing, ", "), howToFix)
	}
}

func TestConfigEnvVarsDocumented(t *testing.T) {
	root := repoRoot(t)
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "config", "config.go"))
	seen := map[string]bool{}
	var vars []string
	ast.Inspect(f, func(n ast.Node) bool {
		bl, ok := n.(*ast.BasicLit)
		if !ok || bl.Kind != token.STRING {
			return true
		}
		s, err := strconv.Unquote(bl.Value)
		if err != nil {
			return true
		}
		if envKey.MatchString(s) && !seen[s] {
			seen[s] = true
			vars = append(vars, s)
		}
		return true
	})
	if len(vars) == 0 {
		t.Fatal("found no SPAWNER_* env vars in config.go — parser broken?")
	}
	doc := readDoc(t, root, "CLAUDE.md")
	reportMissing(t, doc, "CLAUDE.md", vars,
		"Document each SPAWNER_* var in CLAUDE.md's config section (backticked).")
}

func TestInboundMessagesDocumented(t *testing.T) {
	root := repoRoot(t)
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "gateway", "gateway.go"))
	var types []string
	ast.Inspect(f, func(n ast.Node) bool {
		// The dispatch table: `var wireHandlers = map[string]...{ "type": ... }`.
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		isTable := false
		for _, name := range vs.Names {
			if name.Name == "wireHandlers" {
				isTable = true
			}
		}
		if !isTable {
			return true
		}
		for _, v := range vs.Values {
			cl, ok := v.(*ast.CompositeLit)
			if !ok {
				continue
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if s := strLit(kv.Key); s != "" {
					types = append(types, s)
				}
			}
		}
		return true
	})
	if len(types) == 0 {
		t.Fatal("found no inbound message types in gateway.go wireHandlers — parser broken?")
	}
	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	reportMissing(t, doc, "docs/protocol.md", types,
		"Add each inbound type to the App -> server table in docs/protocol.md (backticked).")
}

func TestOutboundMessagesDocumented(t *testing.T) {
	root := repoRoot(t)
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "gateway", "messages.go"))
	seen := map[string]bool{}
	var types []string
	ast.Inspect(f, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range cl.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if strLit(kv.Key) != "type" {
				continue
			}
			if s := strLit(kv.Value); s != "" && !seen[s] {
				seen[s] = true
				types = append(types, s)
			}
		}
		return true
	})
	if len(types) == 0 {
		t.Fatal("found no outbound message types in messages.go — parser broken?")
	}
	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	reportMissing(t, doc, "docs/protocol.md", types,
		"Add each server -> app type to the table in docs/protocol.md (backticked).")
}

func TestErrorCodesDocumented(t *testing.T) {
	root := repoRoot(t)
	gwDir := filepath.Join(root, "server", "internal", "gateway")
	entries, err := os.ReadDir(gwDir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var codes []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		_, f := parseGo(t, filepath.Join(gwDir, name))
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "msgError" || len(call.Args) == 0 {
				return true
			}
			if s := strLit(call.Args[0]); s != "" && !seen[s] {
				seen[s] = true
				codes = append(codes, s)
			}
			return true
		})
	}
	if len(codes) == 0 {
		t.Fatal("found no msgError codes in internal/gateway — parser broken?")
	}
	doc := readDoc(t, root, filepath.Join("docs", "protocol.md"))
	reportMissing(t, doc, "docs/protocol.md", codes,
		"Add each code to the Error codes table in docs/protocol.md (backticked).")
}

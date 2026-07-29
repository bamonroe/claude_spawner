package docsync

// Record field-parity drift checks (the fifth drift dimension). The other tests
// pin message *types* both directions (clientsync_test.go) and payload field
// names docs<->Go (fieldsync_test.go), but nothing checks that each SYNCED
// catalogue record carries the same field set on both ends of the wire. That
// gap lets a Go-side field addition, or a missing "updated_at" on the Kotlin
// side, sail through CI even though it silently breaks the versioned sync layer
// (last-writer-wins + tombstones + the per-catalogue digest). This file closes
// it for all six catalogues (hosts, identities, profiles, providers, settings,
// spoken_tokens).
//
// TestCatalogueRegistryComplete then closes the meta-gap the per-catalogue check
// can't: it asserts the set of catalogues checked here IS the full set registered
// in the digest layer (Go + Kotlin), so a NEW syncable catalogue can't be added
// without a conflict rule — the version-token guarantee, made total.
//
// The parity is framed as the *wire view*: the serialized record that crosses
// the wire in the outbound list message — the exact shape a catalogue's digest
// is folded over — versus the Kotlin read<Type> parser's field set. In that
// framing all five match field-for-field with ZERO exemptions:
//
//   - hosts/profiles/settings: their Go structs are marshaled straight onto the
//     wire, so the struct json tags ARE the wire record.
//   - identities: the Go struct has a server-only `password`; the wire drops it
//     and adds `has_password`, so the record is assembled by hand in
//     msgIdentityList — we read the keys of that map, not the struct.
//   - providers: the wire `agents` object is hand-built in msgAgents from
//     agent.Settings (it is not a marshaled struct), so we read that map's keys.
//
// Both extractors fail loudly on an empty result, so a broken regex/AST walk
// can't silently pass; and both sides must carry the "updated_at" version token.
import (
	"go/ast"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// funcRecordKeys returns the JSON keys of the per-record map[string]any literal a
// message builder in gateway/messages.go serializes onto the wire. It scopes to
// the named function, then among that function's map[string]any composite
// literals selects the wire RECORD: the one carrying the "updated_at" version
// token — which excludes the message envelope (the map bearing "type") and any
// nested sub-object (e.g. a provider's per-model {alias,voice} map, which has no
// updated_at). Used for identities (msgIdentityList) and providers (msgAgents),
// whose wire records deliberately differ from their Go structs. Same AST
// technique as messagesGoWireKeys in fieldsync_test.go, but scoped to one func.
func funcRecordKeys(t *testing.T, root, funcName string) map[string]bool {
	t.Helper()
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "gateway", "messages.go"))
	var fn *ast.FuncDecl
	ast.Inspect(f, func(n ast.Node) bool {
		if fd, ok := n.(*ast.FuncDecl); ok && fd.Name.Name == funcName {
			fn = fd
			return false
		}
		return true
	})
	if fn == nil {
		t.Fatalf("func %s not found in gateway/messages.go — renamed? update recordsync_test.go", funcName)
	}
	isMapStringAny := func(e ast.Expr) bool {
		mt, ok := e.(*ast.MapType)
		if !ok {
			return false
		}
		k, ok := mt.Key.(*ast.Ident)
		if !ok || k.Name != "string" {
			return false
		}
		v, ok := mt.Value.(*ast.Ident)
		return ok && v.Name == "any"
	}
	var records []map[string]bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok || !isMapStringAny(cl.Type) {
			return true
		}
		keys := map[string]bool{}
		for _, elt := range cl.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if k := strLit(kv.Key); k != "" {
					keys[k] = true
				}
			}
		}
		if keys["type"] { // the message envelope, not a record
			return true
		}
		if keys["updated_at"] { // the wire record carries the version token
			records = append(records, keys)
		}
		return true
	})
	if len(records) != 1 {
		t.Fatalf("extractor broken — expected exactly one wire record map (a map[string]any literal "+
			"carrying \"updated_at\", excluding the \"type\" envelope) in %s, found %d", funcName, len(records))
	}
	return records[0]
}

// ktRecordFields returns the field set a Protocol.kt read<Type> parser reads for
// one record. It isolates that function's text (from its `private fun` header to
// the next `private fun` — the read functions and the JsonObject accessor
// extensions that follow are all `private fun`, so this bounds even the last
// one), then regexes the receiver-anchored accessor calls. Anchoring on the
// record's lambda param (recv) is required: it keeps readAgents' nested per-model
// `it.str("alias")`/`it.bool("voice")` accessors out of the AgentInfo field set.
func ktRecordFields(t *testing.T, root, funcName, recv string) map[string]bool {
	t.Helper()
	src := readDoc(t, root, filepath.FromSlash(protocolKt))
	blockRe := regexp.MustCompile(`(?s)private fun ` + regexp.QuoteMeta(funcName) + `\b(.*?)(?:private fun |\z)`)
	m := blockRe.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("Protocol.kt has no `private fun %s` — renamed? update recordsync_test.go", funcName)
	}
	accRe := regexp.MustCompile(`\b` + regexp.QuoteMeta(recv) +
		`\.(?:str|int|long|bool|dbl|strList|strMap|arr)\("([a-z0-9_]+)"\)`)
	out := map[string]bool{}
	for _, mm := range accRe.FindAllStringSubmatch(m[1], -1) {
		out[mm[1]] = true
	}
	return out
}

func toFieldSet(xs []string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRecordFieldParity: for each of the five synced catalogues, the Go wire
// record and the Kotlin read<Type> parser must agree field-for-field, and both
// must carry the "updated_at" version token.
func TestRecordFieldParity(t *testing.T) {
	// One-sided fields go here as "Resource.field" -> reason. The wire-view
	// framing (serialized record vs. read<Type> parser) makes all five match
	// exactly, so this is EMPTY for the current catalogues; kept (with the
	// stale-exemption check below) for the day a genuinely one-sided field lands.
	exempt := map[string]string{}
	usedExempt := map[string]bool{}

	// oneWay returns the fields in a that are absent from b, honoring exemptions
	// (and recording which ones were actually used, so a stale one is caught).
	oneWay := func(name string, a, b map[string]bool) []string {
		var only []string
		for f := range a {
			if b[f] {
				continue
			}
			if key := name + "." + f; exempt[key] != "" {
				usedExempt[key] = true
				continue
			}
			only = append(only, f)
		}
		sort.Strings(only)
		return only
	}

	for _, r := range catalogueRows(t) {
		t.Run(r.name, func(t *testing.T) {
			// Guardrail: a broken extractor (bad regex / AST walk) must not pass silently.
			if len(r.goSet) == 0 {
				t.Fatalf("extractor broken — got 0 fields for %s (Go side)", r.name)
			}
			if len(r.ktSet) == 0 {
				t.Fatalf("extractor broken — got 0 fields for %s (Kotlin %s)", r.name, r.ktFunc)
			}
			t.Logf("%s: go=%v kt=%v", r.name, sortedKeys(r.goSet), sortedKeys(r.ktSet))

			// The version token must be present on BOTH sides — the sync layer's
			// last-writer-wins arbitration and digest depend on it per record.
			if !r.goSet["updated_at"] {
				t.Errorf("%s: the Go wire record has no \"updated_at\" — the sync layer needs the "+
					"version token on every record", r.name)
			}
			if !r.ktSet["updated_at"] {
				t.Errorf("%s: Kotlin %s does not read \"updated_at\" — the app would drop the "+
					"last-writer-wins version token", r.name, r.ktFunc)
			}

			if goOnly := oneWay(r.name, r.goSet, r.ktSet); len(goOnly) > 0 {
				t.Errorf("%s: field(s) on the Go wire record but not read by Kotlin %s: %s\n"+
					"Add them to the parser (or, if intentional, an exemption here with a reason).",
					r.name, r.ktFunc, strings.Join(goOnly, ", "))
			}
			if ktOnly := oneWay(r.name, r.ktSet, r.goSet); len(ktOnly) > 0 {
				t.Errorf("%s: field(s) Kotlin %s reads but the Go wire record never sends: %s\n"+
					"Add them to the wire record (or an exemption here with a reason).",
					r.name, r.ktFunc, strings.Join(ktOnly, ", "))
			}
		})
	}

	for key := range exempt {
		if !usedExempt[key] {
			t.Errorf("stale exemption %q: that field is no longer one-sided — remove the exemption", key)
		}
	}
}

// catalogueRow describes one synced catalogue: its wire name, the Kotlin
// read<Type> parser, and the field sets on each side of the wire.
type catalogueRow struct {
	name   string
	ktFunc string
	goSet  map[string]bool
	ktSet  map[string]bool
}

// catalogueRows is the enumerated set of synced catalogues the parity check
// covers. It is a hand-maintained list — TestCatalogueRegistryComplete cross-checks
// it against the digest layer (the authoritative registry) so a catalogue can't be
// registered there without also being covered here (and thus without its version
// token being asserted on both ends).
func catalogueRows(t *testing.T) []catalogueRow {
	t.Helper()
	root := repoRoot(t)
	sess := filepath.Join(root, "server", "internal", "session")
	return []catalogueRow{
		{"hosts", "readHosts",
			toFieldSet(structJSONTags(t, filepath.Join(sess, "hosts.go"), "Host")),
			ktRecordFields(t, root, "readHosts", "h")},
		{"identities", "readIdentities",
			funcRecordKeys(t, root, "msgIdentityList"),
			ktRecordFields(t, root, "readIdentities", "i")},
		{"profiles", "readProfiles",
			toFieldSet(structJSONTags(t, filepath.Join(sess, "profile.go"), "ExecProfile")),
			ktRecordFields(t, root, "readProfiles", "p")},
		{"spoken_tokens", "readSpokenTokens",
			toFieldSet(structJSONTags(t, filepath.Join(root, "server", "internal", "spoken", "token.go"), "Token")),
			ktRecordFields(t, root, "readSpokenTokens", "t")},
		{"providers", "readAgents",
			funcRecordKeys(t, root, "msgAgents"),
			ktRecordFields(t, root, "readAgents", "a")},
		{"settings", "readSettings",
			toFieldSet(structJSONTags(t, filepath.Join(sess, "settingskv.go"), "SettingRecord")),
			ktRecordFields(t, root, "readSettings", "s")},
	}
}

// normalizeCatalogue folds a catalogue name to a comparison key: lower-case with
// separators stripped, so "spokenTokens" (a Go/Kotlin digest func), "spoken_tokens"
// (a wire/row name) and "spokentokens" all compare equal.
func normalizeCatalogue(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, "_", ""))
}

// bodyReferencesUpdatedAt reports whether a func's body mentions the `UpdatedAt`
// selector anywhere — i.e. the digest folds the version token, so a timestamp-only
// edit still flips the checksum (the whole point of the versioned sync layer).
func bodyReferencesUpdatedAt(fn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "UpdatedAt" {
			found = true
		}
		return !found
	})
	return found
}

// goDigestCatalogues is the AUTHORITATIVE registry of synced catalogues on the Go
// side: every top-level `<catalogue>Digest` func in gateway/catalogdigest.go except
// the generic reducer foldDigest. Each is asserted to fold the `UpdatedAt` version
// token, so a catalogue's digest can't be defined without a conflict rule. Returns
// the normalized catalogue-name set.
func goDigestCatalogues(t *testing.T, root string) map[string]bool {
	t.Helper()
	_, f := parseGo(t, filepath.Join(root, "server", "internal", "gateway", "catalogdigest.go"))
	out := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		if !strings.HasSuffix(name, "Digest") || name == "foldDigest" {
			continue // foldDigest is the shared reducer, not a catalogue
		}
		if !bodyReferencesUpdatedAt(fn) {
			t.Errorf("catalogdigest.go %s does not fold UpdatedAt — a catalogue digest must "+
				"incorporate the version token so a timestamp-only edit flips the checksum", name)
		}
		out[normalizeCatalogue(strings.TrimSuffix(name, "Digest"))] = true
	}
	if len(out) == 0 {
		t.Fatal("extractor broken — no <catalogue>Digest funcs found in catalogdigest.go")
	}
	return out
}

// ktDigestCatalogues is the AUTHORITATIVE registry on the Kotlin side: every public
// fold function in the `object CatalogueDigest` (the private helpers fnv1a/fold/list/
// map are `private fun`, so they're skipped). Each fold is asserted to reference
// `updatedAt`, mirroring the Go-side check. Returns the normalized name set.
func ktDigestCatalogues(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readDoc(t, root, filepath.FromSlash("android/app/src/commonMain/kotlin/com/bam/spawner/net/CatalogueDigest.kt"))
	// A public fold spans `    fun name(` .. its terminating `    })`. Private helpers
	// (`    private fun …`) don't start with "    fun ", so they're excluded.
	blockRe := regexp.MustCompile(`(?sm)^    fun ([a-zA-Z]+)\(.*?\n    \}\)`)
	out := map[string]bool{}
	for _, m := range blockRe.FindAllStringSubmatch(src, -1) {
		name, body := m[1], m[0]
		if !strings.Contains(body, "updatedAt") {
			t.Errorf("CatalogueDigest.%s does not fold updatedAt — the app's digest would drop "+
				"the version token, diverging from the Go fold", name)
		}
		out[normalizeCatalogue(name)] = true
	}
	if len(out) == 0 {
		t.Fatal("extractor broken — no public fold funcs found in CatalogueDigest.kt")
	}
	return out
}

// TestCatalogueRegistryComplete closes the semantic gap the other drift tests can't
// see: it doesn't just check the catalogues we happen to list, it checks that the
// list IS every registered syncable catalogue. The digest layer is the authoritative
// registry — a synced catalogue must have a digest on both ends (the skip-if-equal
// fast path), and each digest is asserted (above) to fold the version token. This
// test requires the Go digests, the Kotlin digests, and the record-parity rows to be
// the SAME set, so registering a new catalogue anywhere without giving it a parity
// row (and thus a both-ends `updated_at` assertion) fails the build.
func TestCatalogueRegistryComplete(t *testing.T) {
	root := repoRoot(t)
	goReg := goDigestCatalogues(t, root)
	ktReg := ktDigestCatalogues(t, root)

	covered := map[string]bool{}
	for _, r := range catalogueRows(t) {
		covered[normalizeCatalogue(r.name)] = true
	}

	// diff reports set membership mismatches both ways with actionable guidance.
	diff := func(regName string, reg map[string]bool) {
		for c := range reg {
			if !covered[c] {
				t.Errorf("catalogue %q has a %s but no record-parity row — a syncable catalogue was "+
					"registered without a conflict rule. Add it to catalogueRows so its `updated_at` "+
					"version token is asserted on both the Go wire record and the Kotlin parser.", c, regName)
			}
		}
		for c := range covered {
			if !reg[c] {
				t.Errorf("catalogue %q has a record-parity row but no %s — every synced catalogue needs "+
					"a digest fold on both ends for the skip-if-equal fast path.", c, regName)
			}
		}
	}
	diff("Go digest (catalogdigest.go)", goReg)
	diff("Kotlin digest (CatalogueDigest.kt)", ktReg)
}

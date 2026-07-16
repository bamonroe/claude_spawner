package gateway

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"

	"github.com/bam/claude_spawner/server/internal/agent"
	"github.com/bam/claude_spawner/server/internal/session"
)

// Per-catalogue digest: a stable, order-INDEPENDENT checksum over the live
// records of one app-managed catalogue (hosts, identities, profiles, providers),
// folding each record's (key, updated_at, payload) so a timestamp-only edit still
// flips it. The app computes the identical value from its cached catalogue
// (android .../net/CatalogueDigest.kt) and presents all four in the `hello`
// handshake; a matching digest lets the server skip re-sending that catalogue on
// connect — the skip-if-equal fast path, mirroring the chat transcript's count+
// hash `digests`/`history unchanged` shortcut, generalized to the catalogues.
//
// The goal is cross-language portability, not cryptographic strength (this is a
// cache-validation checksum a client only ever equality-compares): a 64-bit
// FNV-1a per record, wrapping-summed across records and hex-encoded, is trivial
// to compute byte-for-byte identically in Kotlin's commonMain with no shared
// crypto dependency. Records are keyed uniquely, so the sum never cancels; the
// empty set folds to all-zero.
//
// Tombstones are deliberately NOT folded in. A pending delete the app hasn't yet
// applied simply manifests as a record the app still holds but the server dropped
// — so the two digests already differ, the mismatch ships the (post-delete)
// catalogue, and the app's LWW+tombstone merge removes it. The equal case is only
// ever reached when both sides' *live* records match exactly (including each
// updated_at), which is precisely when doing nothing is correct.

// Separators: control chars that never occur in a name/path/model alias, so the
// canonical record string is unambiguous without length-prefixing.
const (
	digestFieldSep = "\x1f" // between a record's fields (US)
	digestElemSep  = "\x1e" // between list/map elements within one field (RS)
)

// foldDigest reduces per-record canonical strings to one order-independent hex
// digest: FNV-1a-64 each record, wrapping-sum, %016x. Empty input → all zeros.
func foldDigest(records []string) string {
	var sum uint64
	for _, r := range records {
		h := fnv.New64a()
		_, _ = h.Write([]byte(r))
		sum += h.Sum64()
	}
	return fmt.Sprintf("%016x", sum)
}

func digestList(xs []string) string { return strings.Join(xs, digestElemSep) }

func digestMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, digestElemSep)
}

func hostsDigest(hosts []*session.Host) string {
	recs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		recs = append(recs, strings.Join([]string{
			h.Name, h.Address, h.User, strconv.Itoa(h.Port),
			h.KeyFile, h.Identity, h.ClaudeBin, strconv.FormatInt(h.UpdatedAt, 10),
		}, digestFieldSep))
	}
	return foldDigest(recs)
}

// identitiesDigest excludes the server-only password: the app never sees it (only
// a `has_password` flag), so folding it would diverge from the app's digest. A
// password change bumps updated_at, which is folded in, so the digest still flips.
func identitiesDigest(ids []*session.Identity) string {
	recs := make([]string, 0, len(ids))
	for _, id := range ids {
		recs = append(recs, strings.Join([]string{
			id.Name, id.User, id.PublicKey, strconv.FormatInt(id.UpdatedAt, 10),
		}, digestFieldSep))
	}
	return foldDigest(recs)
}

func profilesDigest(profiles []*session.ExecProfile) string {
	recs := make([]string, 0, len(profiles))
	for _, p := range profiles {
		recs = append(recs, strings.Join([]string{
			p.Name, string(p.Target), strconv.FormatBool(p.Default), p.Image, p.HomeMount,
			digestList(p.Mounts), digestList(p.Creds), digestMap(p.Env), digestList(p.RunArgs),
			digestMap(p.Vars), strconv.FormatInt(p.UpdatedAt, 10),
		}, digestFieldSep))
	}
	return foldDigest(recs)
}

// providersDigest folds the exact per-backend content the `agents` message carries
// (id, name, effective default model, model alias list, voice-enabled subset,
// overlay updated_at) so both a compiled-model change and an app overlay edit flip
// it. The server-default-backend marker (reg.Default()) is not app-managed per
// record and is left out.
func providersDigest(reg *agent.Registry, settings *agent.SettingsStore) string {
	recs := make([]string, 0)
	for _, a := range reg.List() {
		cat := a.Catalog()
		models := make([]string, 0, len(cat))
		voice := make([]string, 0, len(cat))
		for _, m := range cat {
			models = append(models, m.Alias)
			if settings.VoiceEnabled(a, m.Alias) {
				voice = append(voice, m.Alias)
			}
		}
		recs = append(recs, strings.Join([]string{
			a.ID, a.Name, settings.DefaultModel(a),
			digestList(models), digestList(voice),
			strconv.FormatInt(settings.UpdatedAt(a), 10),
		}, digestFieldSep))
	}
	return foldDigest(recs)
}

// settingsDigest folds the fifth catalogue — the keyed shared-settings store — with
// the same FNV-1a-64 canonical scheme as the others: each record's (key, value,
// updated_at). Must be byte-identical to the Kotlin CatalogueDigest.settings fold.
func settingsDigest(store *session.SettingKV) string {
	list := store.List()
	recs := make([]string, 0, len(list))
	for _, r := range list {
		recs = append(recs, strings.Join([]string{
			r.Key, r.Value, strconv.FormatInt(r.UpdatedAt, 10),
		}, digestFieldSep))
	}
	return foldDigest(recs)
}

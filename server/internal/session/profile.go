package session

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/template"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExecProfile is a named bundle of execution-environment settings. The persisted
// session stores only Profile (the name); Driver resolves that name to one of
// these before a turn or sandbox lifecycle operation runs.
type ExecProfile struct {
	Name   string `json:"name"`
	Target Target `json:"target,omitempty"`
	// Default marks this profile as the one a session with no explicit choice
	// resolves to. At most one profile is Default; if none is, the first in the
	// catalogue is treated as default. The app sets it (there is no built-in
	// "default" profile — default is a marker, not a special entry).
	Default   bool              `json:"default,omitempty"`
	Image     string            `json:"image,omitempty"`
	HomeMount string            `json:"home_mount,omitempty"`
	Mounts    []string          `json:"mounts,omitempty"`
	Creds     []string          `json:"creds,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	RunArgs   []string          `json:"run_args,omitempty"`
	// Vars are user-defined template values for this profile, overlaid on the
	// server's global vars (profile wins on a name clash). Referenced in the other
	// string fields as {{.Vars.Name}}. Not itself templated.
	Vars map[string]string `json:"vars,omitempty"`
}

// RenderContext is the substitution context for profile templating: three
// built-in host/session-derived values plus the merged user-defined Vars map.
type RenderContext struct {
	Home    string            // login user's home on the executing host
	Session string            // the session's stable name/handle
	Dir     string            // the session's working directory
	Vars    map[string]string // global vars overlaid by the profile's own
}

// expandTemplate renders one {{.Var}} string against ctx. A reference to a
// missing Vars key or an unknown field is a hard error (fail loud — never
// silently expand to an empty path). Strings with no template markers are
// returned untouched so the common case pays nothing.
func expandTemplate(field, s string, ctx RenderContext) (string, error) {
	if !strings.Contains(s, "{{") {
		return s, nil
	}
	t, err := template.New(field).Option("missingkey=error").Parse(s)
	if err != nil {
		return "", fmt.Errorf("profile %s template %q: %w", field, s, err)
	}
	var b strings.Builder
	if err := t.Execute(&b, ctx); err != nil {
		return "", fmt.Errorf("profile %s template %q: %w", field, s, err)
	}
	return b.String(), nil
}

// render returns a copy of p with every string-bearing field (image, home_mount,
// mounts, creds, run_args, and each env value) expanded against ctx. The Vars map
// is the substitution source and is left as-is. p is not mutated.
func (p *ExecProfile) render(ctx RenderContext) (*ExecProfile, error) {
	if p == nil {
		return nil, nil
	}
	out := *p
	var err error
	if out.Image, err = expandTemplate("image", p.Image, ctx); err != nil {
		return nil, err
	}
	if out.HomeMount, err = expandTemplate("home_mount", p.HomeMount, ctx); err != nil {
		return nil, err
	}
	if out.Mounts, err = expandSlice("mounts", p.Mounts, ctx); err != nil {
		return nil, err
	}
	if out.Creds, err = expandSlice("creds", p.Creds, ctx); err != nil {
		return nil, err
	}
	if out.RunArgs, err = expandSlice("run_args", p.RunArgs, ctx); err != nil {
		return nil, err
	}
	if len(p.Env) > 0 {
		out.Env = make(map[string]string, len(p.Env))
		for k, v := range p.Env {
			if out.Env[k], err = expandTemplate("env "+k, v, ctx); err != nil {
				return nil, err
			}
		}
	}
	return &out, nil
}

func expandSlice(field string, in []string, ctx RenderContext) ([]string, error) {
	if len(in) == 0 {
		return in, nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		var err error
		if out[i], err = expandTemplate(field, s, ctx); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// mergeVars overlays profile-level vars on top of the global set; the profile
// wins on a name clash. The result is a fresh map (never aliases either input).
func mergeVars(global, profile map[string]string) map[string]string {
	out := make(map[string]string, len(global)+len(profile))
	for k, v := range global {
		out[k] = v
	}
	for k, v := range profile {
		out[k] = v
	}
	return out
}

// ProfileRegistry is a concurrency-safe, file-backed catalogue of execution
// profiles. The app is the source of truth: it creates/edits/deletes profiles and
// the server persists them to a JSON file and re-broadcasts on change (mirroring
// HostStore / IdentityStore). Exactly one profile may be marked Default; if none
// is, the first in the catalogue is treated as default. There is no built-in
// "default" profile — default is a marker set by the user.
type ProfileRegistry struct {
	path  string
	mu    sync.RWMutex
	order []*ExecProfile
	byID  map[string]*ExecProfile
}

// NewProfileRegistry builds an in-memory registry (no persistence) from the given
// profiles, validating and defensively copying each. Used by tests and by callers
// that construct a Driver literal.
func NewProfileRegistry(profiles ...ExecProfile) (*ProfileRegistry, error) {
	r := &ProfileRegistry{byID: map[string]*ExecProfile{}}
	for _, p := range profiles {
		if err := r.put(p); err != nil {
			return nil, err
		}
	}
	r.normalizeDefault()
	return r, nil
}

// OpenProfileStore loads the profile catalogue from path. A missing file is a
// first run: the store is seeded with `seed` and written out. An existing file is
// read as either a JSON array or a {"profiles":[...]} wrapper.
func OpenProfileStore(path string, seed []ExecProfile) (*ProfileRegistry, error) {
	r := &ProfileRegistry{path: path, byID: map[string]*ExecProfile{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		for _, p := range seed {
			if err := r.put(p); err != nil {
				return nil, err
			}
		}
		r.normalizeDefault()
		return r, r.flush()
	}
	list, err := parseProfiles(data, path)
	if err != nil {
		return nil, err
	}
	for _, p := range list {
		if err := r.put(p); err != nil {
			return nil, err
		}
	}
	r.normalizeDefault()
	return r, nil
}

func parseProfiles(data []byte, path string) ([]ExecProfile, error) {
	var list []ExecProfile
	if err := json.Unmarshal(data, &list); err != nil {
		var wrapped struct {
			Profiles []ExecProfile `json:"profiles"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse profiles %s: %w", path, err)
		}
		list = wrapped.Profiles
	}
	return list, nil
}

// put validates + defensively copies p and upserts it by name. It takes no lock
// and does not persist — callers that mutate (Put/Delete/SetDefault) hold r.mu and
// call flush themselves.
func (r *ProfileRegistry) put(p ExecProfile) error {
	if p.Name == "" {
		return fmt.Errorf("execution profile has empty name")
	}
	if p.Env == nil {
		p.Env = map[string]string{}
	}
	for k := range p.Env {
		if !envNameRE.MatchString(k) {
			return fmt.Errorf("profile %q has invalid env key %q", p.Name, k)
		}
	}
	p.Mounts = append([]string(nil), p.Mounts...)
	p.Creds = append([]string(nil), p.Creds...)
	p.RunArgs = append([]string(nil), p.RunArgs...)
	if p.Vars != nil {
		vars := make(map[string]string, len(p.Vars))
		for k, v := range p.Vars {
			vars[k] = v
		}
		p.Vars = vars
	}
	cp := p
	if _, exists := r.byID[p.Name]; !exists {
		r.order = append(r.order, &cp)
	} else {
		for i, existing := range r.order {
			if existing.Name == p.Name {
				r.order[i] = &cp
				break
			}
		}
	}
	r.byID[p.Name] = &cp
	return nil
}

// normalizeDefault keeps at most one Default marker: if a hand-edited file marks
// several, the first wins and the rest are cleared.
func (r *ProfileRegistry) normalizeDefault() {
	seen := false
	for _, p := range r.order {
		if !p.Default {
			continue
		}
		if seen {
			p.Default = false
		}
		seen = true
	}
}

// Put upserts a profile and persists the catalogue.
func (r *ProfileRegistry) Put(p ExecProfile) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.put(p); err != nil {
		return err
	}
	r.normalizeDefault()
	return r.flush()
}

// Delete removes a profile by name and persists. Removing an absent name is a
// no-op (idempotent, like HostStore.Delete).
func (r *ProfileRegistry) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[name]; !ok {
		return nil
	}
	delete(r.byID, name)
	out := r.order[:0]
	for _, p := range r.order {
		if p.Name != name {
			out = append(out, p)
		}
	}
	r.order = out
	r.normalizeDefault()
	return r.flush()
}

// SetDefault marks name as the default profile, clearing the marker on all others,
// and persists.
func (r *ProfileRegistry) SetDefault(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[name]; !ok {
		return fmt.Errorf("unknown profile %q", name)
	}
	for _, p := range r.order {
		p.Default = p.Name == name
	}
	return r.flush()
}

// Get returns the named profile or nil. The result must not be mutated.
func (r *ProfileRegistry) Get(name string) *ExecProfile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byID[name]
}

// Resolve returns the named profile, falling back to the default (marked, else the
// first) for an empty or unknown name. The result must not be mutated by callers.
func (r *ProfileRegistry) Resolve(name string) *ExecProfile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if name != "" {
		if p := r.byID[name]; p != nil {
			return p
		}
	}
	return r.defaultLocked()
}

// DefaultName is the name of the profile a no-choice session resolves to (the
// marked default, else the first), or "" when the catalogue is empty.
func (r *ProfileRegistry) DefaultName() string {
	if r == nil {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if p := r.defaultLocked(); p != nil {
		return p.Name
	}
	return ""
}

func (r *ProfileRegistry) defaultLocked() *ExecProfile {
	for _, p := range r.order {
		if p.Default {
			return p
		}
	}
	if len(r.order) > 0 {
		return r.order[0]
	}
	return nil
}

// List returns the profiles, default first, then in catalogue order.
func (r *ProfileRegistry) List() []*ExecProfile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*ExecProfile, len(r.order))
	copy(out, r.order)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Default && !out[j].Default })
	return out
}

func (r *ProfileRegistry) flush() error {
	if r.path == "" {
		return nil
	}
	data, err := json.MarshalIndent(map[string]any{"profiles": r.order}, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.path)
}

func (p *ExecProfile) envList() []string {
	if p == nil || len(p.Env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+p.Env[k])
	}
	return out
}

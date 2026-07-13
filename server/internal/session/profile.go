package session

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
)

const DefaultProfileName = "default"

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ExecProfile is a named bundle of execution-environment settings. The persisted
// session stores only Profile (the name); Driver resolves that name to one of
// these before a turn or sandbox lifecycle operation runs.
type ExecProfile struct {
	Name      string            `json:"name"`
	Target    Target            `json:"target,omitempty"`
	Image     string            `json:"image,omitempty"`
	HomeMount string            `json:"home_mount,omitempty"`
	Mounts    []string          `json:"mounts,omitempty"`
	Creds     []string          `json:"creds,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	RunArgs   []string          `json:"run_args,omitempty"`
}

// ProfileRegistry is the ordered execution-profile catalogue advertised to
// clients and used by Driver to resolve a session's Profile name.
type ProfileRegistry struct {
	order []*ExecProfile
	byID  map[string]*ExecProfile
}

// NewProfileRegistry returns a registry containing at least the built-in default
// profile. Additional profiles with duplicate names replace the earlier entry.
func NewProfileRegistry(def ExecProfile, extras ...ExecProfile) (*ProfileRegistry, error) {
	if def.Name == "" {
		def.Name = DefaultProfileName
	}
	r := &ProfileRegistry{byID: map[string]*ExecProfile{}}
	if err := r.add(def, ExecProfile{}); err != nil {
		return nil, err
	}
	base := *r.byID[DefaultProfileName]
	for _, p := range extras {
		if err := r.add(p, base); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// LoadProfiles reads a JSON profile file and overlays it on top of def. A missing
// or empty path leaves only the built-in default profile.
func LoadProfiles(path string, def ExecProfile) (*ProfileRegistry, error) {
	if path == "" {
		return NewProfileRegistry(def)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewProfileRegistry(def)
		}
		return nil, err
	}
	var extras []ExecProfile
	if err := json.Unmarshal(data, &extras); err != nil {
		var wrapped struct {
			Profiles []ExecProfile `json:"profiles"`
		}
		if werr := json.Unmarshal(data, &wrapped); werr != nil {
			return nil, fmt.Errorf("parse profiles %s: %w", path, err)
		}
		extras = wrapped.Profiles
	}
	return NewProfileRegistry(def, extras...)
}

func (r *ProfileRegistry) add(p ExecProfile, base ExecProfile) error {
	if p.Name == "" {
		return fmt.Errorf("execution profile has empty name")
	}
	if p.Image == "" {
		p.Image = base.Image
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
	cp := p
	if _, exists := r.byID[p.Name]; !exists {
		r.order = append(r.order, &cp)
		r.byID[p.Name] = &cp
		return nil
	}
	r.byID[p.Name] = &cp
	for i, existing := range r.order {
		if existing.Name == p.Name {
			r.order[i] = &cp
			break
		}
	}
	return nil
}

// Resolve returns the named profile, falling back to default for empty or unknown
// names. The returned profile must not be mutated by callers.
func (r *ProfileRegistry) Resolve(name string) *ExecProfile {
	if r == nil {
		return nil
	}
	if name != "" {
		if p := r.byID[name]; p != nil {
			return p
		}
	}
	return r.byID[DefaultProfileName]
}

// List returns the profiles in stable display order.
func (r *ProfileRegistry) List() []*ExecProfile {
	if r == nil {
		return nil
	}
	out := append([]*ExecProfile(nil), r.order...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Name == DefaultProfileName {
			return true
		}
		if out[j].Name == DefaultProfileName {
			return false
		}
		return out[i].Name < out[j].Name
	})
	return out
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

package config

import (
	"path/filepath"
	"testing"
)

func TestValidateSpawnDir(t *testing.T) {
	// Two roots, to exercise the multi-root check.
	c := &Config{SpawnRoots: []string{
		filepath.FromSlash("/data/projects"),
		filepath.FromSlash("/home/bam/git"),
	}}

	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"inside first root", filepath.FromSlash("/data/projects/foo"), false},
		{"nested inside first root", filepath.FromSlash("/data/projects/a/b/c"), false},
		{"the root itself", filepath.FromSlash("/data/projects"), false},
		{"inside second root", filepath.FromSlash("/home/bam/git/drat"), false},
		{"escapes via parent", filepath.FromSlash("/data/projects/../etc"), true},
		{"sibling of root", filepath.FromSlash("/data/other"), true},
		{"totally outside", filepath.FromSlash("/etc/passwd"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := c.ValidateSpawnDir(tt.dir)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateSpawnDir(%q): err=%v, wantErr=%v", tt.dir, err, tt.wantErr)
			}
		})
	}
}

func TestValidateSpawnDir_NoRoot(t *testing.T) {
	c := &Config{SpawnRoots: nil}
	if _, err := c.ValidateSpawnDir(filepath.FromSlash("/anywhere/at/all")); err != nil {
		t.Errorf("with no root, any path should be allowed, got %v", err)
	}
}

package pathspeak

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		spoken string
		want   string
	}{
		{"data claude underscore claude", "/data/claude_claude"},
		{"slash data slash projects", "/data/projects"},
		{"data projects foo", "/data/projects/foo"},
		{"home bam dot config", "/home/bam/.config"},
		{"my dash app", "/my-app"},
		{"data", "/data"},
	}
	for _, tt := range tests {
		got, err := Normalize(tt.spoken)
		if err != nil {
			t.Errorf("Normalize(%q) error: %v", tt.spoken, err)
			continue
		}
		if got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.spoken, got, tt.want)
		}
	}
}

func TestNormalizeEmpty(t *testing.T) {
	if _, err := Normalize("   "); err == nil {
		t.Error("expected error for empty input")
	}
}

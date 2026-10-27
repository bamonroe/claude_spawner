package gateway

import (
	"testing"
	"time"

	"github.com/bam/claude_spawner/server/internal/session"
)

func TestStatusText(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cx := func(ago time.Duration, read, write int) *session.ContextSnapshot {
		return &session.ContextSnapshot{
			Usage: session.Usage{Input: 7, CacheRead: read, CacheWrite: write},
			At:    now.Add(-ago).Unix(),
		}
	}
	cases := []struct {
		name     string
		running  bool
		since    time.Time
		activity string
		cx       *session.ContextSnapshot
		want     string
	}{
		{"running", true, now.Add(-2 * time.Minute), "⚙️ running a command…", cx(10*time.Minute, 40_000, 8_200),
			"buddy is working — running a command, for 2 minutes. 48 thousand tokens cached; the cache is warm."},
		{"running before first breadcrumb", true, now.Add(-5 * time.Second), "", nil,
			"buddy is working, for 5 seconds. No cached context yet."},
		{"idle warm", false, time.Time{}, "", cx(3*time.Minute, 120_000, 0),
			"buddy is idle; its last turn finished 3 minutes ago. 120 thousand tokens cached; the cache is warm for another 2 minutes."},
		{"idle cold", false, time.Time{}, "", cx(2*time.Hour, 1_250_000, 0),
			"buddy is idle; its last turn finished 2 hours ago. 1.2 million tokens cached; the cache went cold 1 hour ago."},
		{"idle no timestamp", false, time.Time{}, "", &session.ContextSnapshot{Usage: session.Usage{CacheRead: 512}},
			"buddy is idle. 512 tokens cached; I can't tell if the cache is warm."},
		{"fresh", false, time.Time{}, "", nil,
			"buddy is idle. No cached context yet."},
	}
	for _, tc := range cases {
		if got := statusText("buddy", tc.running, tc.since, tc.activity, tc.cx, now); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestSpokenActivity(t *testing.T) {
	for in, want := range map[string]string{
		"🤔 thinking…":        "thinking",
		"✏️ editing main.go": "editing main.go",
		"· Bash…":            "Bash",
		"":                   "",
	} {
		if got := spokenActivity(in); got != want {
			t.Errorf("spokenActivity(%q) = %q, want %q", in, got, want)
		}
	}
}

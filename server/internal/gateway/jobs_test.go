package gateway

import (
	"testing"

	"github.com/bam/claude_spawner/server/internal/session"
)

// TestTokenCostDiscountsCacheRead guards the usage-estimate weighting: a cache
// READ (the whole cached context re-read each turn) is metered at ~0.1× and must
// not be counted flat, else one big-context turn drifts the estimate ~10× too fast
// and pegs it at 100% after a turn or two. A cache WRITE is ~1.25×.
func TestTokenCostDiscountsCacheRead(t *testing.T) {
	// Fresh input/output count flat; cache read is heavily discounted.
	u := session.Usage{Input: 1000, Output: 2000, CacheWrite: 4000, CacheRead: 1_000_000}
	got := tokenCost(u)
	want := int64(1000 + 2000 + 1.25*4000 + 0.10*1_000_000) // = 108000
	if got != want {
		t.Fatalf("tokenCost = %d, want %d", got, want)
	}

	// The dominant cache-read term must be an order of magnitude below its flat
	// count, so a warm-cache turn no longer dwarfs the seeded session budget.
	flat := int64(u.Input + u.Output + u.CacheWrite + u.CacheRead)
	if got*5 >= flat {
		t.Fatalf("cache read not discounted enough: weighted %d vs flat %d", got, flat)
	}
}

package gateway

import (
	"testing"

	"github.com/bam/claude_spawner/server/internal/command"
)

func kinds(is []command.Intent) []command.Kind {
	ks := make([]command.Kind, len(is))
	for i, in := range is {
		ks[i] = in.Kind
	}
	return ks
}

func TestApplyCancel(t *testing.T) {
	L, D, C := command.Intent{Kind: command.List}, command.Intent{Kind: command.Detach}, command.Intent{Kind: command.Cancel}

	// No cancel: everything survives.
	if got, had := applyCancel([]command.Intent{L, D}); had || len(got) != 2 {
		t.Fatalf("no-cancel: had=%v got=%v", had, kinds(got))
	}
	// Cancel in the middle: only commands after it survive.
	if got, had := applyCancel([]command.Intent{L, C, D}); !had || len(got) != 1 || got[0].Kind != command.Detach {
		t.Fatalf("mid-cancel: had=%v got=%v", had, kinds(got))
	}
	// Trailing cancel: nothing survives.
	if got, had := applyCancel([]command.Intent{L, D, C}); !had || len(got) != 0 {
		t.Fatalf("trailing-cancel: had=%v got=%v", had, kinds(got))
	}
	// Last cancel wins.
	if got, had := applyCancel([]command.Intent{L, C, D, C, L}); !had || len(got) != 1 || got[0].Kind != command.List {
		t.Fatalf("last-cancel-wins: had=%v got=%v", had, kinds(got))
	}
}
